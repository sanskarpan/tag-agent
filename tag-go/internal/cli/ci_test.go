package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// captureStdout runs fn while capturing os.Stdout and returns what was printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestCILoopEchoesPrompt(t *testing.T) {
	root := &cobra.Command{Use: "tag"}
	root.AddGroup(&cobra.Group{ID: "orch", Title: "Orchestration:"})
	registerCI(root, &App{})

	out := captureStdout(t, func() {
		root.SetArgs([]string{"loop", "--provider", "echo", "--iterations", "2", "hello loop"})
		if err := root.Execute(); err != nil {
			t.Fatalf("loop execute: %v", err)
		}
	})
	if strings.Count(out, "hello loop") != 2 {
		t.Errorf("expected prompt echoed twice, got:\n%s", out)
	}
	if !strings.Contains(out, "iteration 1") || !strings.Contains(out, "iteration 2") {
		t.Errorf("expected two iterations, got:\n%s", out)
	}
}

func TestCIAliasRunsOnePass(t *testing.T) {
	root := &cobra.Command{Use: "tag"}
	root.AddGroup(&cobra.Group{ID: "orch", Title: "Orchestration:"})
	registerCI(root, &App{})

	out := captureStdout(t, func() {
		root.SetArgs([]string{"ci", "diagnose this"})
		if err := root.Execute(); err != nil {
			t.Fatalf("ci execute: %v", err)
		}
	})
	if !strings.Contains(out, "diagnose this") {
		t.Errorf("expected task echoed, got:\n%s", out)
	}
}
