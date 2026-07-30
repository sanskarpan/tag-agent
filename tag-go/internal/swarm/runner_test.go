package swarm

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/store"
	"github.com/tag-agent/tag/internal/trace"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "tag.sqlite3"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db.DB); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return db
}

// taskSpec is a compact way to declare a manifest task in a test.
type taskSpec struct {
	id    string
	deps  []string
	reads []string
	write []string
}

func manifestOf(goal string, specs ...taskSpec) *Manifest {
	m := &Manifest{SwarmID: "s", Goal: goal, FailurePolicy: PolicyBestEffort, CoordinatorProfile: "p"}
	for _, s := range specs {
		m.Tasks = append(m.Tasks, Task{
			TaskID: s.id, Description: "do " + s.id, Profile: "p",
			ContextSlice:     ContextSlice{Type: "free_text", Selector: s.id},
			DependsOn:        s.deps,
			ContextBusReads:  s.reads,
			ContextBusWrites: s.write,
		})
	}
	return m
}

// seed persists the run + tasks exactly as `swarm run` does before executing.
func seed(t *testing.T, db *store.DB, swarmID string, m *Manifest, policy string, maxAgents int) {
	t.Helper()
	ctx := context.Background()
	if err := CreateRun(ctx, db.DB, swarmID, m.Goal, "p", policy, maxAgents); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := InsertTasks(ctx, db.DB, swarmID, m.Tasks); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}
}

// scripted is an offline provider that answers each sub-agent deterministically
// and records concurrency. It never touches the network.
type scripted struct {
	fail    map[string]bool // task ids whose agent errors
	hang    map[string]bool // task ids whose agent blocks until ctx is done
	delay   time.Duration
	inFlt   int32
	peak    int32
	started chan string // optional: receives each dispatched task id
}

// taskIDOf recovers the task id from the sub-agent brief buildTaskPrompt renders.
func taskIDOf(prompt string) string {
	const marker = "Your task ("
	i := strings.Index(prompt, marker)
	if i < 0 {
		return ""
	}
	rest := prompt[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	var last string
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			last = m.Content
		}
	}
	id := taskIDOf(last)
	ch := make(chan llm.Event, 4)
	go func() {
		defer close(ch)
		n := atomic.AddInt32(&s.inFlt, 1)
		for {
			p := atomic.LoadInt32(&s.peak)
			if n <= p || atomic.CompareAndSwapInt32(&s.peak, p, n) {
				break
			}
		}
		defer atomic.AddInt32(&s.inFlt, -1)
		if s.started != nil && id != "" {
			select {
			case s.started <- id:
			default:
			}
		}
		if s.hang[id] {
			<-ctx.Done()
			ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
			return
		}
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-ctx.Done():
				ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
				return
			}
		}
		if s.fail[id] {
			ch <- llm.Event{Type: llm.EventError, Err: fmt.Errorf("agent %s exploded", id)}
			return
		}
		ch <- llm.Event{Type: llm.EventTextDelta, Text: "result-of-" + id}
		ch <- llm.Event{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5}}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

func taskRows(t *testing.T, db *store.DB, swarmID string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT task_id, status FROM swarm_tasks WHERE swarm_id=? ORDER BY id`, swarmID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			t.Fatal(err)
		}
		out[id] = st
	}
	return out
}

func runStatus(t *testing.T, db *store.DB, swarmID string) (status string, taskCount int, cost float64) {
	t.Helper()
	var c sql.NullFloat64
	if err := db.QueryRow(`SELECT status, task_count, total_cost_usd FROM swarm_runs WHERE swarm_id=?`, swarmID).
		Scan(&status, &taskCount, &c); err != nil {
		t.Fatalf("run status: %v", err)
	}
	return status, taskCount, c.Float64
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestSwarmRunPersistsRunAndTasks is the headline regression: before this port,
// NOTHING in Go ever wrote swarm_runs/swarm_tasks, so `swarm list/status/results`
// were permanently empty against a Go-only TAG_HOME.
func TestSwarmRunPersistsRunAndTasks(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("goal", taskSpec{id: "a"}, taskSpec{id: "b"})
	seed(t, db, "sw1", m, PolicyBestEffort, 4)

	res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw1"), Options{
		Provider: &scripted{}, MaxAgents: 4, FailurePolicy: PolicyBestEffort,
	}).Run(context.Background(), "sw1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status = %q, want completed (%+v)", res.Status, res)
	}

	// Exactly what `swarm list` / `swarm status` read.
	status, count, _ := runStatus(t, db, "sw1")
	if status != "completed" || count != 2 {
		t.Fatalf("swarm_runs row: status=%q task_count=%d, want completed/2", status, count)
	}
	got := taskRows(t, db, "sw1")
	if got["a"] != "done" || got["b"] != "done" {
		t.Fatalf("swarm_tasks rows = %v, want both done", got)
	}
	// And what `swarm results` reads.
	var out string
	if err := db.QueryRow(`SELECT COALESCE(output,'') FROM swarm_tasks WHERE swarm_id=? AND task_id='a'`, "sw1").Scan(&out); err != nil {
		t.Fatal(err)
	}
	if out != "result-of-a" {
		t.Fatalf("task output = %q, want the agent's final text", out)
	}
	var final string
	if err := db.QueryRow(`SELECT COALESCE(final_output,'') FROM swarm_runs WHERE swarm_id=?`, "sw1").Scan(&final); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "result-of-a") || !strings.Contains(final, "result-of-b") {
		t.Fatalf("final_output %q must synthesize both agents", final)
	}
}

// abort_on_any: the first failure stops the swarm and its dependents are
// recorded as skipped, never left 'pending'.
func TestFailurePolicyAbortOnAny(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b", deps: []string{"a"}})
	seed(t, db, "sw", m, PolicyAbortOnAny, 4)

	res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: &scripted{fail: map[string]bool{"a": true}}, FailurePolicy: PolicyAbortOnAny,
	}).Run(context.Background(), "sw")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	got := taskRows(t, db, "sw")
	if got["a"] != "failed" {
		t.Errorf("task a = %q, want failed", got["a"])
	}
	if got["b"] == "pending" || got["b"] == "running" {
		t.Errorf("task b left in %q — abort_on_any must not strand dependents", got["b"])
	}
}

// best_effort: a failure does not stop the swarm; the run ends 'partial'.
func TestFailurePolicyBestEffortIsPartial(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)

	res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: &scripted{fail: map[string]bool{"a": true}}, FailurePolicy: PolicyBestEffort,
	}).Run(context.Background(), "sw")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "partial" {
		t.Fatalf("status = %q, want partial", res.Status)
	}
	got := taskRows(t, db, "sw")
	if got["a"] != "failed" || got["b"] != "done" {
		t.Fatalf("tasks = %v, want a=failed b=done", got)
	}
}

// require_majority needs strictly MORE than half to succeed.
func TestFailurePolicyRequireMajority(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail map[string]bool
		want string
	}{
		{"minority succeeds -> failed", map[string]bool{"a": true, "b": true}, "failed"},
		{"majority succeeds -> partial", map[string]bool{"a": true}, "partial"},
		{"tie -> failed", map[string]bool{"a": true, "b": true}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b"}, taskSpec{id: "c"})
			seed(t, db, "sw", m, PolicyRequireMajority, 4)
			res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
				Provider: &scripted{fail: tc.fail}, FailurePolicy: PolicyRequireMajority,
			}).Run(context.Background(), "sw")
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.want {
				t.Fatalf("status = %q, want %q", res.Status, tc.want)
			}
			st, _, _ := runStatus(t, db, "sw")
			if st != tc.want {
				t.Fatalf("persisted status = %q, want %q", st, tc.want)
			}
		})
	}
}

// --max-agents bounds real concurrency AND every task still runs.
//
// This is also the regression for the Python scheduler bug we deliberately did
// NOT port: `remaining -= ready` retires every ready task even though only the
// first max_agents were dispatched, so with 6 ready tasks and max_agents=2 four
// of them silently never run while the swarm reports 'completed'.
func TestMaxAgentsBoundsConcurrencyAndRunsEveryTask(t *testing.T) {
	db := openTestDB(t)
	var specs []taskSpec
	for i := 0; i < 6; i++ {
		specs = append(specs, taskSpec{id: fmt.Sprintf("t%d", i)})
	}
	m := manifestOf("g", specs...)
	seed(t, db, "sw", m, PolicyBestEffort, 2)

	p := &scripted{delay: 30 * time.Millisecond}
	res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: p, MaxAgents: 2, FailurePolicy: PolicyBestEffort,
	}).Run(context.Background(), "sw")
	if err != nil {
		t.Fatal(err)
	}
	if peak := atomic.LoadInt32(&p.peak); peak > 2 {
		t.Errorf("peak concurrency %d exceeds --max-agents 2", peak)
	}
	if len(res.Results) != 6 {
		t.Fatalf("%d results, want 6 — tasks were dropped by the scheduler", len(res.Results))
	}
	got := taskRows(t, db, "sw")
	for _, s := range specs {
		if got[s.id] != "done" {
			t.Errorf("task %s = %q, want done (it was never dispatched)", s.id, got[s.id])
		}
	}
	if res.Status != "completed" {
		t.Errorf("status = %q, want completed", res.Status)
	}
}

// --sequential really serialises.
func TestSequentialDispatch(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b"}, taskSpec{id: "c"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	p := &scripted{delay: 20 * time.Millisecond}
	if _, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: p, MaxAgents: 4, Sequential: true,
	}).Run(context.Background(), "sw"); err != nil {
		t.Fatal(err)
	}
	if peak := atomic.LoadInt32(&p.peak); peak != 1 {
		t.Errorf("--sequential peak concurrency = %d, want 1", peak)
	}
}

// writeSame is a provider whose every agent writes the SAME relative filename
// with its own task id as content — the collision shape of #591.
type writeSame struct{ filename string }

func (writeSame) Name() string { return "write-same" }

func (p writeSame) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	var last string
	done := false
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			last = m.Content
		}
		if m.Role == llm.RoleTool {
			done = true
		}
	}
	id := taskIDOf(last)
	ch := make(chan llm.Event, 4)
	go func() {
		defer close(ch)
		if done {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: "wrote " + id}
			ch <- llm.Event{Type: llm.EventFinish}
			return
		}
		ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
			ID: "c1", Name: "write_file",
			Input: map[string]any{"path": p.filename, "content": "content-" + id},
		}}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

// Concurrent sub-agents must not clobber each other's files (#591 applied to the
// swarm topology). Both agents write "out.txt"; both writes must survive.
func TestConcurrentAgentsDoNotShareWorkingDir(t *testing.T) {
	t.Chdir(t.TempDir()) // keep a regression from writing into the repo
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)

	workRoot := t.TempDir()
	if _, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: writeSame{filename: "out.txt"}, MaxAgents: 2, WithTools: true,
		Guard: permission.UnsafeAllowAllGuard(), WorkRoot: workRoot, MaxSteps: 4,
	}).Run(context.Background(), "sw"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		p := filepath.Join(workRoot, "swarm", "sw", id, "out.txt")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("agent %s must have its OWN working dir; %s missing: %v", id, p, err)
		}
		if strings.TrimSpace(string(b)) != "content-"+id {
			t.Fatalf("agent %s wrote %q — agents are sharing a working directory", id, b)
		}
	}
}

// A hostile task_id cannot steer a working directory out of the work root.
func TestTaskWorkDirIsConfined(t *testing.T) {
	root := t.TempDir()
	dir, cleanup, err := taskWorkDir(root, "../../etc", "../../pwn")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		t.Fatalf("task ids steered the work dir out of the root: %q", dir)
	}
}

// SIGTERM/cancellation must not leave a task in 'running' (#574 for swarm).
func TestCancellationLeavesNoTaskRunning(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)

	started := make(chan string, 4)
	p := &scripted{hang: map[string]bool{"a": true, "b": true}, started: started}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Result, 1)
	go func() {
		r, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
			Provider: p, MaxAgents: 2, TimeoutPerAgent: time.Minute,
		}).Run(ctx, "sw")
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- r
	}()
	<-started // at least one agent is in flight
	cancel()

	select {
	case res := <-done:
		if res.Status != "aborted" {
			t.Errorf("status = %q, want aborted", res.Status)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled swarm did not return — SIGTERM must not hang")
	}
	for id, st := range taskRows(t, db, "sw") {
		if st == "running" {
			t.Errorf("task %s left in 'running' after cancellation", id)
		}
	}
	if st, _, _ := runStatus(t, db, "sw"); st == "running" {
		t.Error("swarm_runs left in 'running' after cancellation")
	}
}

// `tag swarm abort` flips swarm_runs.status; a live runner must notice and stop.
func TestOutOfBandAbortStopsRun(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)

	started := make(chan string, 2)
	p := &scripted{hang: map[string]bool{"a": true}, started: started}
	done := make(chan *Result, 1)
	go func() {
		r, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
			Provider: p, TimeoutPerAgent: time.Minute,
		}).Run(context.Background(), "sw")
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- r
	}()
	<-started
	if _, err := db.Exec(`UPDATE swarm_runs SET status='aborted' WHERE swarm_id='sw'`); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.Status != "aborted" {
			t.Errorf("status = %q, want aborted", res.Status)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runner ignored an out-of-band abort")
	}
	for id, st := range taskRows(t, db, "sw") {
		if st == "running" {
			t.Errorf("task %s left 'running' after abort", id)
		}
	}
}

// A sub-agent that overruns --timeout-per-agent is recorded as timed_out.
func TestTimeoutPerAgent(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	if _, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: &scripted{hang: map[string]bool{"a": true}}, TimeoutPerAgent: 100 * time.Millisecond,
	}).Run(context.Background(), "sw"); err != nil {
		t.Fatal(err)
	}
	if got := taskRows(t, db, "sw")["a"]; got != "timed_out" {
		t.Fatalf("task status = %q, want timed_out", got)
	}
}

// Tasks whose dependencies can never be satisfied are stranded VISIBLY as
// skipped (Python B049) rather than silently dropped.
func TestUnsatisfiableDependenciesAreSkipped(t *testing.T) {
	db := openTestDB(t)
	// Hand-built (bypassing Validate) to model a manifest whose dep graph cannot
	// progress, which is exactly the state B049 guards against.
	m := manifestOf("g", taskSpec{id: "a", deps: []string{"ghost"}})
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{Provider: &scripted{}}).
		Run(context.Background(), "sw")
	if err != nil {
		t.Fatal(err)
	}
	if got := taskRows(t, db, "sw")["a"]; got != "skipped" {
		t.Fatalf("task status = %q, want skipped", got)
	}
	if res.Status == "completed" {
		t.Fatal("a swarm with an unrunnable task must not report 'completed'")
	}
}

// The context bus carries an upstream result to a declared downstream reader.
func TestContextBusHandoff(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g",
		taskSpec{id: "up", write: []string{"finding"}},
		taskSpec{id: "down", deps: []string{"up"}, reads: []string{"finding"}})
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	bus := NewContextBus(db.DB, "sw")
	if _, err := NewRunner(db.DB, m, bus, Options{Provider: &scripted{}}).
		Run(context.Background(), "sw"); err != nil {
		t.Fatal(err)
	}
	audit := bus.FullAudit(context.Background())
	if len(audit) != 1 || audit[0].Key != "finding" || audit[0].WrittenBy != "up" {
		t.Fatalf("context bus audit = %+v, want one 'finding' written by 'up'", audit)
	}
	if audit[0].Value != "result-of-up" {
		t.Fatalf("bus value = %v, want the upstream agent output", audit[0].Value)
	}
}

// The bus is write-once per key and rejects unpermitted keys.
func TestContextBusWriteOnceAndPermissions(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	bus := NewContextBus(db.DB, "sw")
	ctx := context.Background()

	if !bus.Write(ctx, "k", "v1", "string", "agent1", []string{"k"}) {
		t.Fatal("first write should succeed")
	}
	if bus.Write(ctx, "k", "v2", "string", "agent2", []string{"k"}) {
		t.Fatal("a second writer must not overwrite another agent's key")
	}
	if !bus.Write(ctx, "k", "v3", "string", "agent1", []string{"k"}) {
		t.Fatal("the owning writer may update its own key")
	}
	if bus.Write(ctx, "other", "v", "string", "agent1", []string{"k"}) {
		t.Fatal("writing outside the permitted key list must be rejected")
	}
	if bus.Write(ctx, "k2", 5, "string", "agent1", []string{"k2"}) {
		t.Fatal("a value that contradicts its declared type must be rejected")
	}
	if bus.Write(ctx, "k3", "v", "weird", "agent1", []string{"k3"}) {
		t.Fatal("an unknown value_type must be rejected")
	}
	snap := bus.ReadSnapshot(ctx, []string{"k"})
	if snap["k"].Value != "v3" {
		t.Fatalf("snapshot = %+v, want v3", snap)
	}
	if len(bus.ReadSnapshot(ctx, nil)) != 0 {
		t.Fatal("an agent with no permitted reads must see an empty snapshot")
	}
}

// A swarm run must be traceable end to end: a swarm.run root with the sub-agent
// loops nested under it.
func TestSwarmEmitsSpans(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	rec := trace.NewRecorder("sw", "p")
	if _, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: &scripted{}, Tracer: rec, Model: "gpt-4o-mini",
	}).Run(context.Background(), "sw"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(db.DB); err != nil {
		t.Fatalf("save spans: %v", err)
	}
	rows, err := db.Query(`SELECT id, COALESCE(parent_id,''), name, kind FROM spans WHERE trace_id='sw'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type rec2 struct{ id, parent, name, kind string }
	byID := map[string]rec2{}
	var rootID string
	agentRuns := 0
	for rows.Next() {
		var r rec2
		if err := rows.Scan(&r.id, &r.parent, &r.name, &r.kind); err != nil {
			t.Fatal(err)
		}
		byID[r.id] = r
		if r.name == "swarm.run" {
			rootID = r.id
		}
		if r.name == "agent.run" {
			agentRuns++
		}
	}
	if rootID == "" {
		t.Fatal("no swarm.run root span recorded")
	}
	if agentRuns < 2 {
		t.Fatalf("%d agent.run spans, want >= 2 (one per sub-agent)", agentRuns)
	}
	for _, r := range byID {
		if r.name == "agent.run" && byID[r.parent].name != "swarm.run" {
			t.Fatalf("agent.run span %s is not nested under the swarm root", r.id)
		}
	}
}

// --approve: 'skip' skips a task and anything that is not y/skip aborts.
func TestApproveGate(t *testing.T) {
	db := openTestDB(t)
	m := manifestOf("g", taskSpec{id: "a"}, taskSpec{id: "b"})
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	var mu sync.Mutex
	res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: &scripted{},
		Approve: func(tk Task) (ApproveDecision, error) {
			mu.Lock()
			defer mu.Unlock()
			if tk.TaskID == "a" {
				return ApproveSkip, nil
			}
			return ApproveDispatch, nil
		},
	}).Run(context.Background(), "sw")
	if err != nil {
		t.Fatal(err)
	}
	got := taskRows(t, db, "sw")
	if got["a"] != "skipped" {
		t.Errorf("task a = %q, want skipped", got["a"])
	}
	if got["b"] != "done" {
		t.Errorf("task b = %q, want done", got["b"])
	}
	if res.Status == "" {
		t.Error("run produced no status")
	}
}

// The degraded (echo/no-manifest) path must be labelled, not silently succeed.
func TestDegradedFlagPropagates(t *testing.T) {
	db := openTestDB(t)
	m := FallbackManifest("do the thing", "sw", "p", "coordinator produced no JSON")
	seed(t, db, "sw", m, PolicyBestEffort, 4)
	res, err := NewRunner(db.DB, m, NewContextBus(db.DB, "sw"), Options{
		Provider: llm.EchoProvider{}, Degraded: true, DegradedReason: "coordinator produced no JSON",
	}).Run(context.Background(), "sw")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded || res.DegradedReason == "" {
		t.Fatalf("degraded run reported as clean: %+v", res)
	}
	if len(res.Results) != 1 || res.Results[0].TaskID != "solo" {
		t.Fatalf("fallback manifest should run exactly one 'solo' task: %+v", res.Results)
	}
}
