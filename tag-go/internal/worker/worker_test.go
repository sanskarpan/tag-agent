package worker

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/store"
)

// openTestDB opens a fresh migrated store DB in a temp dir.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertJob inserts a queue job with the given status and deps.
func insertJob(t *testing.T, db *store.DB, id, status, task string, deps string) {
	t.Helper()
	if deps == "" {
		deps = "[]"
	}
	_, err := db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json)
		VALUES(?,?,?,?,?,5,?,1,?)`, id, "default", task, "mixed", status, time.Now().UTC().Format(time.RFC3339), deps)
	if err != nil {
		t.Fatalf("insert job %s: %v", id, err)
	}
}

// insertJobPriority inserts a dep-free queue job with an explicit priority.
func insertJobPriority(t *testing.T, db *store.DB, id, status, task string, priority int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json)
		VALUES(?,?,?,?,?,?,?,1,'[]')`, id, "default", task, "mixed", status, priority, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert job %s: %v", id, err)
	}
}

func jobStatus(t *testing.T, db *store.DB, id string) (status, result, errText string) {
	t.Helper()
	if err := db.QueryRow(`SELECT status, COALESCE(result,''), COALESCE(error,'') FROM queue_jobs WHERE id=?`, id).
		Scan(&status, &result, &errText); err != nil {
		t.Fatalf("status %s: %v", id, err)
	}
	return
}

// A queued job drains to 'done' with the agent's final text stored as result.
// The echo provider returns the task text, so result must equal the task.
func TestDrainQueuedJobDone(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "j1", "queued", "hello world", "")

	sum, err := Drain(context.Background(), db.DB, Options{})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Claimed != 1 || sum.Done != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want claimed=1 done=1 failed=0", sum)
	}
	status, result, _ := jobStatus(t, db, "j1")
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}
	if result != "hello world" {
		t.Errorf("result = %q, want echoed task", result)
	}
}

// A job with an unmet dependency stays pending and is counted as skipped.
func TestDrainUnmetDepSkipped(t *testing.T) {
	db := openTestDB(t)
	// Dependency 'ghost' does not exist -> never 'done', never terminal-failed.
	insertJob(t, db, "child", "pending", "needs ghost", `["ghost"]`)

	sum, err := Drain(context.Background(), db.DB, Options{})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Claimed != 0 || sum.Done != 0 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want no claims/done/fails", sum)
	}
	if sum.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", sum.Skipped)
	}
	if status, _, _ := jobStatus(t, db, "child"); status != "pending" {
		t.Errorf("status = %q, want pending (unchanged)", status)
	}
}

// A job whose dependency has failed is cascade-failed (not left pending).
func TestDrainCascadeFailOnFailedDep(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "parent", "failed", "boom", "")
	insertJob(t, db, "child", "pending", "depends on parent", `["parent"]`)

	sum, err := Drain(context.Background(), db.DB, Options{})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Failed != 1 {
		t.Fatalf("summary = %+v, want failed=1", sum)
	}
	status, _, errText := jobStatus(t, db, "child")
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if errText == "" {
		t.Errorf("expected cascade-fail error message, got empty")
	}
}

// A dependent runs after its dependency completes in the same Drain (DAG chain).
func TestDrainResolvesDependencyChain(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "a", "ready", "step a", "")
	insertJob(t, db, "b", "pending", "step b", `["a"]`)

	sum, err := Drain(context.Background(), db.DB, Options{})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Claimed != 2 || sum.Done != 2 {
		t.Fatalf("summary = %+v, want claimed=2 done=2", sum)
	}
	if s, _, _ := jobStatus(t, db, "b"); s != "done" {
		t.Errorf("dependent status = %q, want done", s)
	}
}

// Concurrent Drain calls must not double-execute a single job: exactly one
// drainer claims it, so the total claimed/done across all drains is 1.
func TestConcurrentDrainNoDoubleExecute(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "solo", "queued", "run once", "")

	const n = 8
	var wg sync.WaitGroup
	sums := make([]Summary, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			s, err := Drain(context.Background(), db.DB, Options{})
			if err != nil {
				t.Errorf("drain %d: %v", idx, err)
			}
			sums[idx] = s
		}(i)
	}
	close(start)
	wg.Wait()

	totalClaimed, totalDone := 0, 0
	for _, s := range sums {
		totalClaimed += s.Claimed
		totalDone += s.Done
	}
	if totalClaimed != 1 {
		t.Errorf("total claimed = %d across %d drains, want exactly 1", totalClaimed, n)
	}
	if totalDone != 1 {
		t.Errorf("total done = %d, want exactly 1", totalDone)
	}
	if s, _, _ := jobStatus(t, db, "solo"); s != "done" {
		t.Errorf("final status = %q, want done", s)
	}
}

// MaxJobs caps how many jobs a single Drain executes.
func TestDrainMaxJobsCap(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "q1", "queued", "one", "")
	insertJob(t, db, "q2", "queued", "two", "")
	insertJob(t, db, "q3", "queued", "three", "")

	sum, err := Drain(context.Background(), db.DB, Options{MaxJobs: 2})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Claimed != 2 || sum.Done != 2 {
		t.Fatalf("summary = %+v, want claimed=2 done=2 (capped)", sum)
	}
}

// Higher-priority jobs are claimed before older lower-priority ones.
func TestDrainHonorsPriority(t *testing.T) {
	db := openTestDB(t)
	insertJobPriority(t, db, "low", "queued", "low prio", 1)
	insertJobPriority(t, db, "high", "queued", "high prio", 9)

	sum, err := Drain(context.Background(), db.DB, Options{MaxJobs: 1})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Claimed != 1 || sum.Done != 1 {
		t.Fatalf("summary = %+v, want claimed=1 done=1", sum)
	}
	if s, _, _ := jobStatus(t, db, "high"); s != "done" {
		t.Errorf("high-priority status = %q, want done", s)
	}
	if s, _, _ := jobStatus(t, db, "low"); s != "queued" {
		t.Errorf("low-priority status = %q, want queued (untouched)", s)
	}
}

// OnlyJobs restricts a Drain to the given ids; unrelated queued jobs are left alone.
func TestDrainOnlyJobs(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "unrelated", "queued", "old job", "")
	insertJob(t, db, "target", "queued", "new job", "")

	sum, err := Drain(context.Background(), db.DB, Options{OnlyJobs: []string{"target"}})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Claimed != 1 || sum.Done != 1 {
		t.Fatalf("summary = %+v, want claimed=1 done=1", sum)
	}
	if s, _, _ := jobStatus(t, db, "target"); s != "done" {
		t.Errorf("target status = %q, want done", s)
	}
	if s, _, _ := jobStatus(t, db, "unrelated"); s != "queued" {
		t.Errorf("unrelated status = %q, want queued (untouched)", s)
	}
}

// finish must not overwrite a job that was cancelled while it was executing.
func TestFinishPreservesCancelledStatus(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "c", "cancelled", "cancelled mid-run", "")
	if err := ensureResultColumn(db.DB); err != nil {
		t.Fatalf("ensure result column: %v", err)
	}

	applied, err := finish(db.DB, "c", "done", "result", "")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if applied {
		t.Errorf("finish applied over a cancelled job, want no-op")
	}
	if s, _, _ := jobStatus(t, db, "c"); s != "cancelled" {
		t.Errorf("status = %q, want cancelled (preserved)", s)
	}
}

// cancelProvider cancels the drain context mid-run, then fails the job, to
// simulate an interrupt landing while a job is executing.
type cancelProvider struct{ cancel context.CancelFunc }

func (cancelProvider) Name() string { return "cancel" }
func (p cancelProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.cancel()
	return nil, errors.New("interrupted")
}

// A job whose run fails after the drain context is cancelled must still be
// recorded as failed rather than left in 'running' forever.
func TestDrainCancelledContextRecordsFailure(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "j", "queued", "gets interrupted", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sum, err := Drain(ctx, db.DB, Options{Provider: cancelProvider{cancel: cancel}})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Failed != 1 {
		t.Fatalf("summary = %+v, want failed=1", sum)
	}
	status, _, errText := jobStatus(t, db, "j")
	if status != "failed" {
		t.Errorf("status = %q, want failed (not stuck running)", status)
	}
	if errText == "" {
		t.Errorf("expected failure reason recorded")
	}
}

// A job stuck in 'running' past the stale-claim lease is requeued and executed;
// a recently claimed running job is left alone.
func TestDrainReclaimsStaleRunning(t *testing.T) {
	db := openTestDB(t)
	insertJob(t, db, "stale", "queued", "abandoned by crashed worker", "")
	insertJob(t, db, "fresh", "queued", "actively running elsewhere", "")
	old := time.Now().UTC().Add(-2 * staleClaimLease).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE queue_jobs SET status='running', started_at=? WHERE id='stale'`, old); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	recent := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE queue_jobs SET status='running', started_at=? WHERE id='fresh'`, recent); err != nil {
		t.Fatalf("mark fresh: %v", err)
	}

	sum, err := Drain(context.Background(), db.DB, Options{})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Claimed != 1 || sum.Done != 1 {
		t.Fatalf("summary = %+v, want claimed=1 done=1 (stale reclaimed)", sum)
	}
	if s, _, _ := jobStatus(t, db, "stale"); s != "done" {
		t.Errorf("stale status = %q, want done (reclaimed and executed)", s)
	}
	if s, _, _ := jobStatus(t, db, "fresh"); s != "running" {
		t.Errorf("fresh status = %q, want running (untouched)", s)
	}
}
