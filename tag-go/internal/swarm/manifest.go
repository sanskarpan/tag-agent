// Package swarm implements PRD-004/PRD-023 context-centric multi-agent swarm
// orchestration for the Go runtime.
//
// It is a port of src/tag/swarm.py, adapted to Go's in-process runtime:
//
//   - Python has no agent loop of its own, so it decomposes a goal by shelling
//     out to `hermes chat` and then runs every sub-agent as a `python -m
//     tag.swarm_agent_entry` SUBPROCESS, exchanging JSON through 0600 temp files
//     and reaping PIDs with killpg. All of that machinery exists only to cross a
//     process boundary.
//   - Go owns its runtime (internal/agent, internal/llm, internal/tool), so a
//     sub-agent is just an agent.Loop run on a goroutine. A bounded pool of
//     MaxAgents goroutines reproduces the observable behaviour of Python's
//     ThreadPoolExecutor(max_workers=max_agents)-over-subprocesses without temp
//     files, PIDs or signal handling — and, unlike the subprocess model, it
//     cancels cleanly through context.
//
// What is preserved verbatim: the manifest schema and every validation rule, the
// write-once context bus, wave-based dependency scheduling, the three failure
// policies, the synthesis step, and the swarm_runs/swarm_tasks/swarm_context
// tables the `swarm list/status/results` commands read.
package swarm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// MaxAgents is the hard, non-negotiable ceiling on concurrent sub-agents
// (Python: SWARM_MAX_AGENTS).
const MaxAgents = 10

// DefaultMaxAgents mirrors the Python --max-agents default.
const DefaultMaxAgents = 4

// Failure policies (Python: _VALID_FAILURE_POLICIES).
const (
	PolicyAbortOnAny      = "abort_on_any"
	PolicyBestEffort      = "best_effort"
	PolicyRequireMajority = "require_majority"
)

// ValidFailurePolicies lists the accepted --failure-policy values in the order
// Python's argparse `choices` tuple declares them.
var ValidFailurePolicies = []string{PolicyAbortOnAny, PolicyBestEffort, PolicyRequireMajority}

// ValidFailurePolicy reports whether p is an accepted failure policy.
func ValidFailurePolicy(p string) bool {
	for _, v := range ValidFailurePolicies {
		if v == p {
			return true
		}
	}
	return false
}

// validSliceTypes mirrors Python's _VALID_SLICE_TYPES.
var validSliceTypes = map[string]bool{
	"file_paths": true, "directory": true, "url_list": true,
	"key_list": true, "free_text": true,
}

// ManifestError is the Go equivalent of SwarmManifestError: the coordinator
// output is not a usable task manifest.
type ManifestError struct{ msg string }

func (e ManifestError) Error() string { return e.msg }

func manifestErrf(format string, a ...any) error {
	return ManifestError{fmt.Sprintf(format, a...)}
}

// ContextSlice is a task's non-overlapping slice of the world.
type ContextSlice struct {
	Type     string `json:"type"`
	Selector any    `json:"selector,omitempty"`
}

// Task is one sub-agent assignment in the manifest.
type Task struct {
	TaskID           string       `json:"task_id"`
	Description      string       `json:"description"`
	ContextSlice     ContextSlice `json:"context_slice"`
	Profile          string       `json:"profile"`
	DependsOn        []string     `json:"depends_on,omitempty"`
	ContextBusReads  []string     `json:"context_bus_reads,omitempty"`
	ContextBusWrites []string     `json:"context_bus_writes,omitempty"`
}

// Manifest is the coordinator's decomposition of a goal.
type Manifest struct {
	SwarmID            string `json:"swarm_id"`
	Goal               string `json:"goal"`
	Tasks              []Task `json:"tasks"`
	FailurePolicy      string `json:"failure_policy,omitempty"`
	SynthesisProfile   string `json:"synthesis_profile,omitempty"`
	CoordinatorProfile string `json:"coordinator_profile,omitempty"`
}

// taskIDRe mirrors Python's `tid.replace("_","").replace("-","").isalnum()`
// check: alphanumerics plus underscore and dash, and at least one alphanumeric
// (Python rejects "" and "___" because "".isalnum() is False).
var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var taskIDHasAlnum = regexp.MustCompile(`[A-Za-z0-9]`)

// ParseManifest decodes and validates coordinator output.
//
// Presence of the required keys is checked against the raw JSON object, not the
// decoded struct: Go would silently zero a missing "goal" where Python raises,
// and a manifest that quietly loses its goal is exactly the kind of fake success
// this port must not introduce.
func ParseManifest(data []byte, maxAgents int) (*Manifest, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, manifestErrf("Coordinator output is not valid JSON: %v", err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, manifestErrf("Manifest must be a JSON object")
	}
	var missing []string
	for _, k := range []string{"swarm_id", "goal", "tasks"} {
		if _, ok := obj[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, manifestErrf("Manifest missing required keys: %v", missing)
	}
	rawTasks, ok := obj["tasks"].([]any)
	if !ok || len(rawTasks) == 0 {
		return nil, manifestErrf("Manifest 'tasks' must be a non-empty array")
	}
	for i, rt := range rawTasks {
		tm, ok := rt.(map[string]any)
		if !ok {
			return nil, manifestErrf("Task %d must be a JSON object", i)
		}
		var tmissing []string
		for _, k := range []string{"task_id", "description", "context_slice", "profile"} {
			if _, ok := tm[k]; !ok {
				tmissing = append(tmissing, k)
			}
		}
		if len(tmissing) > 0 {
			sort.Strings(tmissing)
			return nil, manifestErrf("Task missing keys %v (task %d)", tmissing, i)
		}
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, manifestErrf("Coordinator output is not a well-typed manifest: %v", err)
	}
	if err := Validate(&m, maxAgents); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate applies every rule from Python's _validate_manifest.
func Validate(m *Manifest, maxAgents int) error {
	if len(m.Tasks) == 0 {
		return manifestErrf("Manifest 'tasks' must be a non-empty array")
	}
	if len(m.Tasks) > maxAgents {
		return manifestErrf("Manifest has %d tasks but max_agents=%d", len(m.Tasks), maxAgents)
	}
	seen := map[string]bool{}
	var allSelectors []string
	for _, t := range m.Tasks {
		if !taskIDRe.MatchString(t.TaskID) || !taskIDHasAlnum.MatchString(t.TaskID) {
			return manifestErrf("task_id must be alphanumeric+underscore+dash: %q", t.TaskID)
		}
		if seen[t.TaskID] {
			return manifestErrf("Duplicate task_id: %s", t.TaskID)
		}
		seen[t.TaskID] = true
		if !validSliceTypes[t.ContextSlice.Type] {
			return manifestErrf("Invalid context_slice.type for task %s", t.TaskID)
		}
		// Overlap detection applies to LIST selectors only (a free_text selector
		// is a string and is never compared), exactly as Python does.
		if sel, ok := t.ContextSlice.Selector.([]any); ok {
			for _, s := range sel {
				key := fmt.Sprintf("%v", s)
				for _, prev := range allSelectors {
					if prev == key {
						return manifestErrf("Overlapping context_slice selector %q in task %s", key, t.TaskID)
					}
				}
				allSelectors = append(allSelectors, key)
			}
		}
	}
	depMap := map[string][]string{}
	for _, t := range m.Tasks {
		depMap[t.TaskID] = t.DependsOn
		var unknown []string
		for _, d := range t.DependsOn {
			if !seen[d] {
				unknown = append(unknown, d)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return manifestErrf("Task %q depends_on unknown task_id(s): %v", t.TaskID, unknown)
		}
	}
	return assertAcyclic(depMap)
}

// assertAcyclic is the white/grey/black DFS from Python's _assert_acyclic.
func assertAcyclic(depMap map[string][]string) error {
	visited := map[string]bool{}
	inStack := map[string]bool{}
	// Iterate in sorted order so the reported cycle member is deterministic
	// (Python iterates a dict, whose order is insertion order; sorting here makes
	// the error message stable regardless of manifest ordering).
	nodes := make([]string, 0, len(depMap))
	for n := range depMap {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	var dfs func(string) error
	dfs = func(node string) error {
		visited[node] = true
		inStack[node] = true
		for _, dep := range depMap[node] {
			if inStack[dep] {
				return manifestErrf("Dependency cycle detected involving task %q", node)
			}
			if !visited[dep] {
				if err := dfs(dep); err != nil {
					return err
				}
			}
		}
		delete(inStack, node)
		return nil
	}
	for _, n := range nodes {
		if !visited[n] {
			if err := dfs(n); err != nil {
				return err
			}
		}
	}
	return nil
}
