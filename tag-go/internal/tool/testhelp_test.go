package tool

import (
	"context"
	"fmt"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
)

// runNamed drives a single tool through a one-shot agent.Loop using a provider
// that emits exactly one tool call for `name`, then verifies the executed result.
func runNamed(t *testing.T, reg *agent.Registry, name string, in map[string]any) (string, error) {
	t.Helper()
	prov := &oneShotProvider{name: name, input: in}
	l := &agent.Loop{Provider: prov, Tools: reg}
	res, err := l.Run(context.Background(), "run", agent.Options{MaxSteps: 2})
	if err != nil {
		return "", err
	}
	if len(res.Steps) == 0 || len(res.Steps[0].ToolCalls) == 0 {
		return "", fmt.Errorf("no tool executed")
	}
	ec := res.Steps[0].ToolCalls[0]
	if ec.Err != "" {
		return ec.Result, fmt.Errorf("%s", ec.Err)
	}
	return ec.Result, nil
}
