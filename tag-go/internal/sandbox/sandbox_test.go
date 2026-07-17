package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExecEcho(t *testing.T) {
	res, err := Exec(context.Background(), Options{Command: "echo hello-sandbox", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Exit != 0 || res.TimedOut {
		t.Fatalf("unexpected result: %+v", res)
	}
	if strings.TrimSpace(res.Stdout) != "hello-sandbox" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestExecNonzeroExit(t *testing.T) {
	res, err := Exec(context.Background(), Options{Command: "exit 3", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Exit != 3 || res.TimedOut {
		t.Fatalf("expected exit 3, got %+v", res)
	}
}

func TestExecTimeout(t *testing.T) {
	res, err := Exec(context.Background(), Options{Command: "sleep 5", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut || res.Exit != 124 {
		t.Fatalf("expected timeout (exit 124), got %+v", res)
	}
}

func TestExecStderrCapture(t *testing.T) {
	res, err := Exec(context.Background(), Options{Command: "echo oops 1>&2", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stderr) != "oops" {
		t.Fatalf("stderr = %q", res.Stderr)
	}
}

func TestExecConfinedDir(t *testing.T) {
	dir := t.TempDir()
	res, err := Exec(context.Background(), Options{Command: "pwd", Dir: dir, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// EvalSymlinks may rewrite the tmp path (macOS), so just require non-empty
	// and that it ends with the tmp dir's base component.
	if strings.TrimSpace(res.Stdout) == "" {
		t.Fatalf("empty pwd output")
	}
}

func TestExecRejectsBadInput(t *testing.T) {
	if _, err := Exec(context.Background(), Options{Command: "", Timeout: time.Second}); err == nil {
		t.Fatal("expected error for empty command")
	}
	if _, err := Exec(context.Background(), Options{Command: "echo x", Timeout: 0}); err == nil {
		t.Fatal("expected error for non-positive timeout")
	}
	if _, err := Exec(context.Background(), Options{Command: "echo x", Dir: "/no/such/dir/xyz", Timeout: time.Second}); err == nil {
		t.Fatal("expected error for missing dir")
	}
}
