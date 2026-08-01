package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PRD-112: conditional edges and state reducers for the DAG engine.
//
// The engine this extends is a job queue, not an in-process graph interpreter:
// `dag run` writes one queue_jobs row per step and a drainer executes rows whose
// dependencies have reached 'done'. Conditional edges and reducers are therefore
// expressed as per-JOB metadata (a Flow, stored in queue_jobs.flow_json) that the
// drainer evaluates at dispatch time, which keeps every existing property of the
// engine intact: dependency-ordered waves, independent nodes dispatched in the
// same pass, one working directory per job, SIGTERM leaving nothing in 'running',
// and integer dependency indices validated up front.
//
// Two concepts, both evaluated against the run's REDUCED STATE:
//
//   - A conditional edge is a Guard on the dependent node. When the guard is
//     false the node is not executed; it goes terminal-'skipped' and every node
//     behind it is skipped too, so the untaken branch of a fork genuinely does
//     not run (see cascadeSkip in worker.go). 'skipped' is a status the existing
//     `dag show` already renders.
//   - A state reducer says how repeated writes to the same state key combine.
//     Each node publishes its result under an output key; when several nodes
//     share a key, Reduce folds them in step order. A node's task text may
//     reference the reduced value with {{state.<key>}}, which is what makes the
//     merged state load-bearing rather than merely observable.
//
// State values are strings because a job's result is a string. `sum` and `merge`
// parse on demand and REPORT a parse failure rather than silently degrading to a
// concatenation — a reducer that quietly changed meaning would be exactly the
// fake success this project forbids.

// Flow is the per-job workflow metadata stored in queue_jobs.flow_json. A job
// without one behaves exactly as it did before this feature existed.
type Flow struct {
	// RunID groups every job submitted by one `dag run` invocation. State is
	// reduced per run, so two runs of the same DAG never see each other's values.
	RunID string `json:"run_id"`
	// DAG is the saved DAG's name (for `dag state` display).
	DAG string `json:"dag,omitempty"`
	// Index is the step's position in the spec: reducers fold in this order, so
	// a merged value is deterministic rather than dependent on completion order.
	Index int `json:"idx"`
	// Name is the step name, when the spec gave one.
	Name string `json:"name,omitempty"`
	// Output is the state key this node's result is published under.
	Output string `json:"output"`
	// Reduce names the reducer for Output (empty = ReduceLast).
	Reduce string `json:"reduce,omitempty"`
	// When, when set, is the conditional edge guarding this node.
	When *Guard `json:"when,omitempty"`
}

// Guard is one conditional edge: a predicate over a single state key.
type Guard struct {
	// Source is the state key to test. `dag run` resolves the spec's `source`
	// (which may be omitted when the node has exactly one dependency) to a
	// concrete key before storing it, so the drainer never has to re-derive it.
	Source string `json:"source"`
	// Op is one of the ops in guardOps.
	Op string `json:"op"`
	// Value is the comparison operand (unused by the arity-1 ops).
	Value string `json:"value,omitempty"`
}

// Reducer names. These are the only accepted values of a step's `reduce` key.
const (
	ReduceLast   = "last"   // a later write replaces an earlier one (default)
	ReduceFirst  = "first"  // the first write wins
	ReduceAppend = "append" // newline-joined, in step order
	ReduceConcat = "concat" // joined with no separator, in step order
	ReduceSum    = "sum"    // numeric addition
	ReduceMerge  = "merge"  // shallow merge of JSON objects
)

// reducers is the reducer registry. Keeping it a map means ValidReduce and
// ReducerNames cannot drift from what Reduce actually implements.
var reducers = map[string]func(prev, next string) (string, error){
	ReduceLast:   func(_, next string) (string, error) { return next, nil },
	ReduceFirst:  func(prev, _ string) (string, error) { return prev, nil },
	ReduceAppend: func(prev, next string) (string, error) { return prev + "\n" + next, nil },
	ReduceConcat: func(prev, next string) (string, error) { return prev + next, nil },
	ReduceSum:    reduceSum,
	ReduceMerge:  reduceMerge,
}

// ValidReduce reports whether kind names a reducer ("" means the default).
func ValidReduce(kind string) bool {
	if kind == "" {
		return true
	}
	_, ok := reducers[kind]
	return ok
}

// ReducerNames lists the reducers, sorted, for error messages.
func ReducerNames() []string {
	out := make([]string, 0, len(reducers))
	for k := range reducers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reduceSum adds two decimal numbers. A non-numeric operand is an ERROR, not a
// fallback to string concatenation: silently changing what `sum` means would
// hand back a plausible-looking value that is not a sum.
func reduceSum(prev, next string) (string, error) {
	a, err := strconv.ParseFloat(strings.TrimSpace(prev), 64)
	if err != nil {
		return "", fmt.Errorf("reduce=sum: %q is not a number", prev)
	}
	b, err := strconv.ParseFloat(strings.TrimSpace(next), 64)
	if err != nil {
		return "", fmt.Errorf("reduce=sum: %q is not a number", next)
	}
	return strconv.FormatFloat(a+b, 'f', -1, 64), nil
}

// reduceMerge shallow-merges two JSON objects (next wins on a key clash). A
// non-object operand is an error, for the same reason as reduceSum.
func reduceMerge(prev, next string) (string, error) {
	var a, b map[string]any
	if err := json.Unmarshal([]byte(prev), &a); err != nil {
		return "", fmt.Errorf("reduce=merge: %q is not a JSON object", truncateForErr(prev))
	}
	if err := json.Unmarshal([]byte(next), &b); err != nil {
		return "", fmt.Errorf("reduce=merge: %q is not a JSON object", truncateForErr(next))
	}
	for k, v := range b {
		a[k] = v
	}
	out, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// truncateForErr bounds a value quoted into an error message.
func truncateForErr(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:57] + "..."
}

// Reduce folds next into prev using the named reducer.
func Reduce(kind, prev, next string) (string, error) {
	if kind == "" {
		kind = ReduceLast
	}
	fn, ok := reducers[kind]
	if !ok {
		return "", fmt.Errorf("unknown reducer %q (want one of %v)", kind, ReducerNames())
	}
	return fn(prev, next)
}

// guardOps is the conditional-edge operator registry. arity1 ops ignore Value.
var guardOps = map[string]struct {
	arity1 bool
	eval   func(got, want string) (bool, error)
}{
	"equals":       {false, func(g, w string) (bool, error) { return g == w, nil }},
	"not_equals":   {false, func(g, w string) (bool, error) { return g != w, nil }},
	"contains":     {false, func(g, w string) (bool, error) { return strings.Contains(g, w), nil }},
	"not_contains": {false, func(g, w string) (bool, error) { return !strings.Contains(g, w), nil }},
	"prefix":       {false, func(g, w string) (bool, error) { return strings.HasPrefix(g, w), nil }},
	"suffix":       {false, func(g, w string) (bool, error) { return strings.HasSuffix(g, w), nil }},
	"matches": {false, func(g, w string) (bool, error) {
		re, err := regexp.Compile(w)
		if err != nil {
			return false, fmt.Errorf("op=matches: %q is not a valid regular expression: %w", w, err)
		}
		return re.MatchString(g), nil
	}},
	"empty":     {true, func(g, _ string) (bool, error) { return strings.TrimSpace(g) == "", nil }},
	"non_empty": {true, func(g, _ string) (bool, error) { return strings.TrimSpace(g) != "", nil }},
}

// ValidOp reports whether op names a guard operator, and whether it takes a
// `value` operand.
func ValidOp(op string) (ok, needsValue bool) {
	spec, ok := guardOps[op]
	if !ok {
		return false, false
	}
	return true, !spec.arity1
}

// GuardOpNames lists the guard operators, sorted, for error messages.
func GuardOpNames() []string {
	out := make([]string, 0, len(guardOps))
	for k := range guardOps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Eval decides whether the guarded node runs. A guard whose source key is not
// present in the state is FALSE, never an error: the producing branch may itself
// have been skipped, and that is precisely the "this branch was not taken" case.
func (g *Guard) Eval(state map[string]string) (bool, error) {
	if g == nil {
		return true, nil
	}
	spec, ok := guardOps[g.Op]
	if !ok {
		return false, fmt.Errorf("unknown guard op %q (want one of %v)", g.Op, GuardOpNames())
	}
	got, present := state[g.Source]
	if !present {
		return false, nil
	}
	return spec.eval(got, g.Value)
}

// stateRefRe matches a {{state.<key>}} reference in a task. The key charset is
// deliberately narrow so the pattern can never swallow surrounding text.
var stateRefRe = regexp.MustCompile(`\{\{\s*state\.([A-Za-z0-9_.-]+)\s*\}\}`)

// StateRefs lists the state keys a task references, in order of appearance.
func StateRefs(task string) []string {
	var out []string
	for _, m := range stateRefRe.FindAllStringSubmatch(task, -1) {
		out = append(out, m[1])
	}
	return out
}

// Interpolate substitutes {{state.<key>}} references in task from state.
//
// A reference to a key the run has not produced is an ERROR. Substituting an
// empty string would hand the model a silently truncated prompt and record the
// result as a success, which is the exact failure mode this project forbids;
// the job fails instead, naming the key.
func Interpolate(task string, state map[string]string) (string, error) {
	var missing []string
	out := stateRefRe.ReplaceAllStringFunc(task, func(m string) string {
		key := stateRefRe.FindStringSubmatch(m)[1]
		v, ok := state[key]
		if !ok {
			missing = append(missing, key)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("task references state key(s) not produced by this run: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// stateEntry is one completed node's contribution to the reduced state.
type stateEntry struct {
	index  int
	key    string
	reduce string
	value  string
}

// RunState computes the reduced state of a DAG run from the nodes that have
// reached 'done'. Nodes that failed, were cancelled or were skipped contribute
// nothing — a value that was never produced must not appear in the state.
//
// Reduction is deterministic: entries fold in step-index order, not completion
// order, so re-reading the state of a finished run always gives the same answer.
func RunState(ctx context.Context, db *sql.DB, runID string) (map[string]string, error) {
	if err := ensureFlowColumn(db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(flow_json,''), COALESCE(result,''), status
		FROM queue_jobs WHERE flow_json IS NOT NULL AND flow_json != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []stateEntry
	for rows.Next() {
		var flowJSON, result, status string
		if err := rows.Scan(&flowJSON, &result, &status); err != nil {
			return nil, err
		}
		if status != "done" {
			continue
		}
		var f Flow
		if err := json.Unmarshal([]byte(flowJSON), &f); err != nil {
			continue // a row we cannot parse contributes nothing
		}
		if f.RunID != runID || f.Output == "" {
			continue
		}
		entries = append(entries, stateEntry{index: f.Index, key: f.Output, reduce: f.Reduce, value: result})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].index < entries[j].index })

	state := map[string]string{}
	for _, e := range entries {
		prev, seen := state[e.key]
		if !seen {
			state[e.key] = e.value
			continue
		}
		merged, err := Reduce(e.reduce, prev, e.value)
		if err != nil {
			return nil, fmt.Errorf("reducing state key %q: %w", e.key, err)
		}
		state[e.key] = merged
	}
	return state, nil
}

// LatestRunID returns the most recent DAG run id, or "" when no run has ever
// been submitted. Used by `dag state` with no argument.
func LatestRunID(ctx context.Context, db *sql.DB) (string, error) {
	if err := ensureFlowColumn(db); err != nil {
		return "", err
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(flow_json,'') FROM queue_jobs
		WHERE flow_json IS NOT NULL AND flow_json != '' ORDER BY created_at DESC, id DESC LIMIT 200`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var flowJSON string
		if err := rows.Scan(&flowJSON); err != nil {
			return "", err
		}
		var f Flow
		if err := json.Unmarshal([]byte(flowJSON), &f); err == nil && f.RunID != "" {
			return f.RunID, nil
		}
	}
	return "", rows.Err()
}

// RunExists reports whether any job belongs to runID, so `dag state` can refuse
// an unknown run instead of printing an empty map as though it were an answer.
func RunExists(ctx context.Context, db *sql.DB, runID string) (bool, error) {
	if err := ensureFlowColumn(db); err != nil {
		return false, err
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(flow_json,'') FROM queue_jobs
		WHERE flow_json IS NOT NULL AND flow_json != ''`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var flowJSON string
		if err := rows.Scan(&flowJSON); err != nil {
			return false, err
		}
		var f Flow
		if err := json.Unmarshal([]byte(flowJSON), &f); err == nil && f.RunID == runID {
			return true, nil
		}
	}
	return false, rows.Err()
}

// RunNodes returns the per-node view of a run (step order), for `dag state`.
type RunNode struct {
	JobID  string `json:"job_id"`
	Index  int    `json:"index"`
	Name   string `json:"name,omitempty"`
	Output string `json:"output"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// RunNodes lists a run's nodes in step order.
func RunNodes(ctx context.Context, db *sql.DB, runID string) ([]RunNode, error) {
	if err := ensureFlowColumn(db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(flow_json,''), status, COALESCE(error,'')
		FROM queue_jobs WHERE flow_json IS NOT NULL AND flow_json != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunNode{}
	for rows.Next() {
		var id, flowJSON, status, errText string
		if err := rows.Scan(&id, &flowJSON, &status, &errText); err != nil {
			return nil, err
		}
		var f Flow
		if err := json.Unmarshal([]byte(flowJSON), &f); err != nil || f.RunID != runID {
			continue
		}
		out = append(out, RunNode{JobID: id, Index: f.Index, Name: f.Name,
			Output: f.Output, Status: status, Reason: errText})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}
