package ciauto

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// IterationResult captures one agent-loop pass.
type IterationResult struct {
	Iteration int
	FinalText string
	Stopped   string
	Steps     int
}

// providerNames returns the registered provider slugs, sorted (for error hints).
func providerNames() []string {
	names := make([]string, 0, len(llm.Registry))
	for k := range llm.Registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// RunLoop runs the native agent loop up to `iterations` times using the named
// provider (default "echo" is offline). It returns one result per iteration.
// It performs NO live calls unless the caller selects a live provider.
func RunLoop(ctx context.Context, providerName, prompt string, iterations int) ([]IterationResult, error) {
	if providerName == "" {
		providerName = "echo"
	}
	if iterations < 1 {
		iterations = 1
	}
	prov := llm.Registry[providerName]
	if prov == nil {
		return nil, fmt.Errorf("unknown provider %q (available: %s)",
			providerName, strings.Join(providerNames(), ", "))
	}
	l := &agent.Loop{Provider: prov}
	out := make([]IterationResult, 0, iterations)
	for i := 0; i < iterations; i++ {
		res, err := l.Run(ctx, prompt, agent.Options{MaxSteps: 8})
		if err != nil {
			return out, err
		}
		out = append(out, IterationResult{
			Iteration: i + 1,
			FinalText: res.FinalText,
			Stopped:   res.Stopped,
			Steps:     len(res.Steps),
		})
	}
	return out, nil
}
