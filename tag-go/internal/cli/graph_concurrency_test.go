package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
)

// TestE2EGraphBuildConcurrentMentionCount is the regression guard for the
// lost-update race in graph build: `graph.Reset` + the extract loop were not
// transactional and `addEntity` did a read-modify-write on mention_count, so N
// concurrent `graph build` runs inflated mention_count instead of converging on
// the memory count. Every run rebuilds from scratch, so whichever run commits
// last must leave mention_count == number of seeded memories.
func TestE2EGraphBuildConcurrentMentionCount(t *testing.T) {
	h := newHome(t)
	const nMem = 60
	for i := 0; i < nMem; i++ {
		if out, code := run(t, h, "mem", "add",
			fmt.Sprintf("Memory %d about Python and Docker and Kubernetes deployment", i)); code != 0 {
			t.Fatalf("seed %d: %s", i, out)
		}
	}

	const nProcs = 8
	var wg sync.WaitGroup
	outs := make([]string, nProcs)
	codes := make([]int, nProcs)
	for i := 0; i < nProcs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(tagBin, "graph", "build")
			cmd.Env = append(os.Environ(), "TAG_HOME="+h)
			b, err := cmd.CombinedOutput()
			outs[i] = string(b)
			if ee, ok := err.(*exec.ExitError); ok {
				codes[i] = ee.ExitCode()
			}
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 {
			t.Fatalf("graph build #%d exited %d: %s", i, codes[i], outs[i])
		}
	}

	out, code := run(t, h, "--json", "graph", "show")
	if code != 0 {
		t.Fatalf("graph show: %s", out)
	}
	var got struct {
		Entities []struct {
			Name         string `json:"name"`
			MentionCount int    `json:"mention_count"`
		} `json:"entities"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode graph show: %v (%s)", err, out)
	}
	if len(got.Entities) == 0 {
		t.Fatalf("no entities after concurrent builds: %s", out)
	}
	for _, e := range got.Entities {
		if e.MentionCount != nMem {
			t.Errorf("entity %q mention_count=%d, want %d (lost-update corruption)", e.Name, e.MentionCount, nMem)
		}
	}
}
