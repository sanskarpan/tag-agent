package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// queueStatuses returns id->status for every job in the queue.
func queueStatuses(t *testing.T, home string) map[string]string {
	t.Helper()
	out, code := run(t, home, "--json", "queue", "list")
	if code != 0 {
		t.Fatalf("queue list: %s", out)
	}
	var jobs []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &jobs); err != nil {
		t.Fatalf("decode queue list: %v (%s)", err, out)
	}
	m := map[string]string{}
	for _, j := range jobs {
		m[j.ID] = j.Status
	}
	return m
}

// startAndSigterm launches tag with a stalled provider so the job blocks
// mid-flight, sends SIGTERM once it is definitely in the agent loop, and waits
// for the process to leave.
func startAndSigterm(t *testing.T, home, base string, args ...string) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home, "TAG_LOCAL_BASE_URL="+base)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	time.Sleep(2 * time.Second) // let it claim the job and enter the provider call
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("process ignored SIGTERM")
	}
}

// TestE2EDagRunExecuteHandlesSigterm is the regression guard for stranded jobs:
// `dag run --execute` passed a bare context.Background() to worker.Drain, so
// SIGTERM killed the process with the job still marked 'running'. It then stayed
// that way for the whole 30-minute staleClaimLease, blocking dependents.
// `queue worker` already did this correctly via signal.NotifyContext.
func TestE2EDagRunExecuteHandlesSigterm(t *testing.T) {
	h := newHome(t)
	base := stalledListener(t)
	if out, code := run(t, h, "dag", "save", "d1", "--steps", `[{"task":"do a thing"}]`); code != 0 {
		t.Fatalf("dag save: %s", out)
	}
	startAndSigterm(t, h, base, "dag", "run", "d1", "--execute", "--provider", "local")

	for id, st := range queueStatuses(t, h) {
		if st == "running" {
			t.Errorf("job %s stranded in 'running' after SIGTERM (want a terminal status)", id)
		}
	}
}

// TestE2ECronRunExecuteHandlesSigterm is the same guard for `cron run --execute`.
func TestE2ECronRunExecuteHandlesSigterm(t *testing.T) {
	h := newHome(t)
	base := stalledListener(t)
	if out, code := run(t, h, "cron", "add", "do a thing", "--name", "j1", "--schedule", "* * * * *"); code != 0 {
		t.Fatalf("cron add: %s", out)
	}
	out, code := run(t, h, "--json", "cron", "list")
	if code != 0 {
		t.Fatalf("cron list: %s", out)
	}
	var jobs []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &jobs); err != nil || len(jobs) != 1 {
		t.Fatalf("decode cron list: %v (%s)", err, out)
	}
	startAndSigterm(t, h, base, "cron", "run", jobs[0].ID, "--execute", "--provider", "local")

	for id, st := range queueStatuses(t, h) {
		if st == "running" {
			t.Errorf("job %s stranded in 'running' after SIGTERM (want a terminal status)", id)
		}
	}
}
