package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
)

// writeFileProvider is an offline provider that always answers with a single
// write_file tool call targeting the SAME relative filename, using the job's own
// task text as the content. Two jobs driven by it therefore write identical
// paths with different content — which is exactly the collision #591 describes.
type writeFileProvider struct{ filename string }

func (p writeFileProvider) Name() string { return "write-file-mock" }

func (p writeFileProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	// The last user message is the job's task text.
	content := ""
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			content = m.Content
		}
	}
	// A tool result is already in the transcript => the write happened; finish.
	done := false
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			done = true
		}
	}
	ch := make(chan llm.Event, 4)
	go func() {
		defer close(ch)
		if done {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: "wrote " + content}
			ch <- llm.Event{Type: llm.EventFinish}
			return
		}
		ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
			ID:    "call_1",
			Name:  "write_file",
			Input: map[string]any{"path": p.filename, "content": content},
		}}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

// TestConcurrentJobsDoNotShareWorkingDir is the regression test for #591.
//
// Two independent jobs are drained CONCURRENTLY (as `dag run --execute` does for
// independent nodes) and both write the same relative filename. Before the fix
// tool.Options.Root was left at "" for every job, so tools resolved against the
// worker process's cwd and the two jobs wrote the SAME file: one job's content
// silently overwrote the other's. After the fix each job has its own root, so
// both writes survive with their own content.
func TestConcurrentJobsDoNotShareWorkingDir(t *testing.T) {
	// Keep the pre-fix (shared-cwd) behaviour from writing into the repo.
	t.Chdir(t.TempDir())

	db := openTestDB(t)
	insertJob(t, db, "job-a", "queued", "alpha-content", "")
	insertJob(t, db, "job-b", "queued", "beta-content", "")

	workRoot := t.TempDir()
	opts := Options{
		Provider:  writeFileProvider{filename: "out.txt"},
		WithTools: true,
		Guard:     permission.UnsafeAllowAllGuard(),
		WorkRoot:  workRoot,
		MaxSteps:  4,
	}

	// Drain each job in its own goroutine: OnlyJobs keeps the two drainers from
	// racing for the same row, so both really do execute in parallel.
	errs := make(chan error, 2)
	for _, id := range []string{"job-a", "job-b"} {
		go func(id string) {
			o := opts
			o.OnlyJobs = []string{id}
			_, err := Drain(context.Background(), db.DB, o)
			errs <- err
		}(id)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("drain: %v", err)
		}
	}

	for _, tc := range []struct{ job, want string }{
		{"job-a", "alpha-content"},
		{"job-b", "beta-content"},
	} {
		if status, _, errText := jobStatus(t, db, tc.job); status != "done" {
			t.Fatalf("%s: status=%s err=%s", tc.job, status, errText)
		}
		p := filepath.Join(workRoot, tc.job, "out.txt")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s must have its OWN working dir; %s not written: %v", tc.job, p, err)
		}
		if got := strings.TrimSpace(string(b)); got != tc.want {
			t.Fatalf("%s wrote %q, want %q — jobs are sharing a working directory", tc.job, got, tc.want)
		}
	}
}

// TestJobWorkDirIsPerJobAndConfined covers the directory contract itself: the
// path is derived from the job id under WorkRoot, and a hostile id cannot steer
// the root elsewhere.
func TestJobWorkDirIsPerJobAndConfined(t *testing.T) {
	root := t.TempDir()
	a, cleanA, err := jobWorkDir(root, "job-a")
	if err != nil {
		t.Fatal(err)
	}
	b, cleanB, err := jobWorkDir(root, "job-b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two jobs got the same dir %q", a)
	}
	for _, d := range []string{a, b} {
		if !strings.HasPrefix(d, root+string(os.PathSeparator)) {
			t.Fatalf("job dir %q escaped work root %q", d, root)
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("job dir %q not created: %v", d, err)
		}
	}
	// A traversal-flavoured id must stay inside the work root.
	esc, cleanEsc, err := jobWorkDir(root, "../../etc/pwn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(esc, root+string(os.PathSeparator)) {
		t.Fatalf("job id steered the work dir out of the root: %q", esc)
	}
	// Cleanup removes an untouched dir but keeps one holding artifacts.
	if err := os.WriteFile(filepath.Join(b, "artifact.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanA()
	cleanB()
	cleanEsc()
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Errorf("empty job dir %q should be cleaned up", a)
	}
	if _, err := os.Stat(b); err != nil {
		t.Errorf("job dir with artifacts must be kept: %v", err)
	}
}

// TestWorkerEmitsSpans is the worker half of the #590 regression: a drained job
// must leave an agent->llm->tool span tree in the DB under the job id.
func TestWorkerEmitsSpans(t *testing.T) {
	t.Chdir(t.TempDir())
	db := openTestDB(t)
	insertJob(t, db, "job-span", "queued", "trace-me", "")

	_, err := Drain(context.Background(), db.DB, Options{
		Provider:  writeFileProvider{filename: "out.txt"},
		WithTools: true,
		Guard:     permission.UnsafeAllowAllGuard(),
		WorkRoot:  t.TempDir(),
		Model:     "openai/gpt-4o-mini",
		MaxSteps:  4,
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	rows, err := db.Query(`SELECT id, COALESCE(parent_id,''), name, kind, status FROM spans WHERE trace_id='job-span' ORDER BY started_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type rec struct{ id, parent, name, kind, status string }
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.parent, &r.name, &r.kind, &r.status); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) == 0 {
		t.Fatal("worker recorded NO spans for the job it ran (#590)")
	}
	byID := map[string]rec{}
	for _, r := range got {
		byID[r.id] = r
	}
	var toolSpan, llmSpan *rec
	for i := range got {
		switch got[i].kind {
		case "tool":
			toolSpan = &got[i]
		case "llm":
			if llmSpan == nil {
				llmSpan = &got[i]
			}
		}
	}
	if llmSpan == nil {
		t.Fatal("no llm span recorded for the job")
	}
	if toolSpan == nil {
		t.Fatal("no tool span recorded for the job's write_file call (PRD-048)")
	}
	if toolSpan.name != "write_file" {
		t.Errorf("tool span name = %q, want write_file", toolSpan.name)
	}
	parent, ok := byID[toolSpan.parent]
	if !ok || parent.kind != "llm" {
		t.Fatalf("tool span parent %q is not the llm turn that requested it", toolSpan.parent)
	}
	if parent.parent == "" {
		t.Fatal("the llm span must itself be a child of the agent.run root span")
	}
	if root, ok := byID[parent.parent]; !ok || root.kind != "agent" {
		t.Fatalf("llm span parent %q is not the agent root", parent.parent)
	}
}

// TestSpanAttributesAreJSON guards the attributes column contract the trace
// snapshot/replay/diff commands parse.
func TestSpanAttributesAreJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	db := openTestDB(t)
	insertJob(t, db, "job-attrs", "queued", "hello", "")
	if _, err := Drain(context.Background(), db.DB, Options{Model: "gpt-4o-mini"}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	rows, err := db.Query(`SELECT attributes FROM spans WHERE trace_id='job-attrs'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var attrs string
		if err := rows.Scan(&attrs); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(attrs), &m); err != nil {
			t.Fatalf("attributes %q is not JSON: %v", attrs, err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("an echo-provider job must still record spans")
	}
}
