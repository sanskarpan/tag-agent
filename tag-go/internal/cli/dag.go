package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/worker"
)

// The `dag` command group: save/run/show/list, plus the PRD-112 additions
// (conditional edges, state reducers and the read-only `dag state`). It used to
// live inline in registerQueue; it moved here when the flow layer made it the
// larger half of that file.
//
// PRD-112 surface decision. The PRD sketches a `tag workflow graph` group over
// workflows compiled into the binary and registered by name. That is a second,
// disjoint engine: the DAG that actually ships is SPEC-driven (a JSON step array
// stored in queue_dags, executed as queue_jobs rows), and a compiled-in registry
// would give users no way to author a workflow at all. Conditional edges and
// state reducers are therefore added to the engine that exists, as three new
// optional step keys (`when`, `output`, `reduce`) plus `dag state`. This
// deliberately does not touch `tag plan decompose` (PRD-105) and leaves the
// `tag workflow …` namespace free for PRD-109's interrupt.

// depRefString renders a DAG dependency reference for an error message.
//
// Dependencies are step INDEXES the user writes as JSON integers, but they are
// decoded into `any` as float64, and %v switches float64 to scientific notation
// past ~1e7 — so `depends_on: [999999999]` was echoed back as "9.99999999e+08",
// which matches nothing in the user's input. Integral values are therefore
// printed as integers; everything else keeps its natural rendering so a
// genuinely malformed reference (1.5, true, {}) is still shown verbatim.
func depRefString(ref any) string {
	if f, ok := ref.(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", ref)
}

// dagRecognizedKeys is the closed set of step keys. `when`/`output`/`reduce` are
// the PRD-112 additions; everything else is unchanged. The set stays closed (an
// unknown key is rejected) for the reason it always was: a typo'd key that is
// silently ignored drops an edge the user believed they had drawn.
var dagRecognizedKeys = map[string]bool{
	"name": true, "task": true, "depends_on": true, "profile": true, "task_type": true,
	"when": true, "output": true, "reduce": true,
}

// dagDepAliases are dependency keys from other engines. They are rejected with a
// pointed hint rather than the generic unknown-key message.
var dagDepAliases = map[string]bool{
	"deps": true, "depends": true, "needs": true, "dependencies": true, "requires": true, "after": true,
}

// stateKeyRe constrains a state key to the charset {{state.<key>}} can address,
// so a key that is written can always be read back.
var stateKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// dagStep is one validated step: the raw spec plus everything the engine derived
// from it (dependency indices, the resolved output key, the resolved guard).
type dagStep struct {
	Index    int
	Name     string
	Task     string
	Profile  string
	TaskType string
	DepIdx   []int
	// DepCount is how many dependency references the spec WROTE. It can exceed
	// len(DepIdx) in non-strict mode, where an unresolvable reference is left for
	// `dag run` to report.
	DepCount int
	Flow     worker.Flow
}

// defaultOutputKey is the state key a step publishes under when it does not name
// one: its step name, or `step<index>`. Deriving it (rather than requiring
// `output`) is what lets a guard reference a plain, unannotated step.
func defaultOutputKey(name string, index int) string {
	if name != "" && stateKeyRe.MatchString(name) {
		return name
	}
	return fmt.Sprintf("step%d", index)
}

// buildDagSteps validates a spec and derives the executable form of every step.
// It is the single implementation used by BOTH `dag save` and `dag run`, so a
// spec can never pass save and then fail at dispatch on anything save could
// have checked.
//
// strict draws the one deliberate line between the two callers, and it is the
// PRE-EXISTING split, kept intact: `dag save` has never resolved dependency
// references (an out-of-range index or an unknown step name is reported by `dag
// run`, and there are regression tests pinning exactly those messages), so in
// non-strict mode an unresolvable dependency is carried forward untouched rather
// than rejected. Everything else — the closed key set, task shape, output keys,
// reducers, guards and state references — is checked identically in both modes.
//
// runID is stamped into each step's Flow. `dag save` passes "" because it is not
// submitting anything.
func buildDagSteps(dagName string, steps []map[string]any, runID string, strict bool) ([]dagStep, error) {
	nameToIdx := map[string]int{}
	for i, s := range steps {
		if nm, ok := s["name"].(string); ok && nm != "" {
			if prev, dup := nameToIdx[nm]; dup {
				return nil, fmt.Errorf("step %d reuses the name %q already used by step %d; "+
					"names address steps in depends_on, so they must be unique", i, nm, prev)
			}
			nameToIdx[nm] = i
		}
	}

	out := make([]dagStep, 0, len(steps))
	// outputOwner maps a state key to the reducer agreed for it, so two steps
	// writing one key with DIFFERENT reducers is caught rather than resolved
	// arbitrarily at runtime.
	outputOwner := map[string]string{}
	produced := map[string]bool{}

	for i, s := range steps {
		for k := range s {
			if dagRecognizedKeys[k] {
				continue
			}
			if dagDepAliases[k] {
				return nil, fmt.Errorf("step %d uses unrecognized dependency key %q; use 'depends_on' instead", i, k)
			}
			return nil, fmt.Errorf("step %d has unrecognized key %q; allowed keys are %s", i, k, dagAllowedKeysList())
		}
		taskVal, ok := s["task"]
		if !ok {
			return nil, fmt.Errorf("step %d is missing required non-empty 'task'", i)
		}
		taskStr, isStr := taskVal.(string)
		if !isStr || strings.TrimSpace(taskStr) == "" {
			return nil, fmt.Errorf("step %d 'task' must be a non-empty string", i)
		}
		st := dagStep{Index: i, Task: taskStr}
		st.Name, _ = s["name"].(string)
		st.Profile, _ = s["profile"].(string)
		st.TaskType, _ = s["task_type"].(string)

		// --- dependencies (unchanged semantics: integral indices or step names,
		// strictly earlier than this step) ---
		if raw, ok := s["depends_on"]; ok && raw != nil {
			list, isList := raw.([]any)
			if !isList {
				return nil, fmt.Errorf("DAG %q step %d 'depends_on' must be a list", dagName, i)
			}
			// depCount counts the references the user WROTE, not the ones that
			// resolved, so a guard's "does this step depend on anything?" check
			// gives the same answer in strict and non-strict mode.
			st.DepCount = len(list)
			for _, ref := range list {
				var idx int
				var refErr error
				switch v := ref.(type) {
				case float64:
					// A dependency is a step INDEX, so only integral values are
					// meaningful. int(1.5) used to truncate to 1 and the step
					// silently depended on the wrong one (or on itself).
					if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
						refErr = fmt.Errorf("DAG %q step %d has an invalid dependency %s", dagName, i, depRefString(ref))
					} else {
						idx = int(v)
					}
				case string:
					j, ok := nameToIdx[v]
					if !ok {
						refErr = fmt.Errorf("DAG %q step %d depends on unknown step %q", dagName, i, v)
					} else {
						idx = j
					}
				default:
					refErr = fmt.Errorf("DAG %q step %d has an invalid dependency %s", dagName, i, depRefString(ref))
				}
				if refErr == nil && idx == i {
					refErr = fmt.Errorf("DAG %q step %d cannot depend on itself", dagName, i)
				}
				if refErr == nil && (idx < 0 || idx >= i) {
					refErr = fmt.Errorf("DAG %q step %d depends on step %s, which is not an earlier step", dagName, i, depRefString(ref))
				}
				if refErr != nil {
					if strict {
						return nil, refErr
					}
					continue // reported by `dag run`, as it always has been
				}
				st.DepIdx = append(st.DepIdx, idx)
			}
		}

		// --- output key + reducer ---
		outKey := defaultOutputKey(st.Name, i)
		if raw, ok := s["output"]; ok {
			v, isStr := raw.(string)
			if !isStr || strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("step %d 'output' must be a non-empty string", i)
			}
			if !stateKeyRe.MatchString(v) {
				return nil, fmt.Errorf("step %d 'output' %q is not a valid state key "+
					"(letters, digits, dot, dash, underscore)", i, v)
			}
			outKey = v
		}
		reduce := ""
		if raw, ok := s["reduce"]; ok {
			v, isStr := raw.(string)
			if !isStr {
				return nil, fmt.Errorf("step %d 'reduce' must be a string", i)
			}
			if !worker.ValidReduce(v) {
				return nil, fmt.Errorf("step %d has unknown 'reduce' %q; allowed reducers are %v", i, v, worker.ReducerNames())
			}
			reduce = v
		}
		if prev, seen := outputOwner[outKey]; seen && prev != reduce {
			return nil, fmt.Errorf("step %d writes state key %q with 'reduce' %q, but an earlier step writes "+
				"the same key with %q; steps sharing an output key must agree on the reducer",
				i, outKey, orDefaultReduce(reduce), orDefaultReduce(prev))
		}
		outputOwner[outKey] = reduce
		produced[outKey] = true

		// --- conditional edge ---
		if raw, ok := s["when"]; ok && raw != nil {
			g, err := buildGuard(i, raw, st, steps, produced)
			if err != nil {
				return nil, err
			}
			st.Flow.When = g
		}

		st.Flow.RunID = runID
		st.Flow.DAG = dagName
		st.Flow.Index = i
		st.Flow.Name = st.Name
		st.Flow.Output = outKey
		st.Flow.Reduce = reduce
		out = append(out, st)
	}

	// A {{state.<key>}} reference must name a key some step produces. Checking it
	// here means an unresolvable reference is a save-time error, not a job that
	// fails halfway through a run.
	for _, st := range out {
		for _, ref := range worker.StateRefs(st.Task) {
			if !produced[ref] {
				return nil, fmt.Errorf("step %d references {{state.%s}}, but no step in this DAG produces "+
					"the state key %q (produced keys: %s)", st.Index, ref, ref, strings.Join(sortedSet(produced), ", "))
			}
		}
	}
	return out, nil
}

// orDefaultReduce renders an empty reducer as its documented default, so the
// conflict message never says `""`.
func orDefaultReduce(r string) string {
	if r == "" {
		return worker.ReduceLast + " (default)"
	}
	return r
}

// dagAllowedKeysList renders the recognized-key set for the unknown-key error.
func dagAllowedKeysList() string {
	return "[" + strings.Join(sortedSet(dagRecognizedKeys), " ") + "]"
}

// buildGuard validates and resolves one step's `when` block.
//
// `source` names a STATE KEY, and it is resolved to a concrete key here rather
// than at dispatch time so the drainer never has to re-derive it (and so an
// unresolvable one is rejected at save). When the step has exactly one
// dependency the key defaults to that dependency's output; with several
// dependencies it is required, because guessing which parent a guard means is
// exactly the silent wrong-edge bug the closed key set exists to prevent.
func buildGuard(i int, raw any, st dagStep, steps []map[string]any, produced map[string]bool) (*worker.Guard, error) {
	obj, isObj := raw.(map[string]any)
	if !isObj {
		return nil, fmt.Errorf("step %d 'when' must be an object like "+
			`{"source":"<state key>","op":"contains","value":"..."}`, i)
	}
	if st.DepCount == 0 {
		return nil, fmt.Errorf("step %d has a 'when' guard but no 'depends_on'; a conditional edge tests "+
			"state produced by an earlier step, so the step must depend on one", i)
	}
	for k := range obj {
		switch k {
		case "source", "op", "value":
		default:
			return nil, fmt.Errorf("step %d 'when' has unrecognized key %q; allowed keys are [op source value]", i, k)
		}
	}
	op, _ := obj["op"].(string)
	if op == "" {
		return nil, fmt.Errorf("step %d 'when' is missing 'op'; allowed ops are %v", i, worker.GuardOpNames())
	}
	valid, needsValue := worker.ValidOp(op)
	if !valid {
		return nil, fmt.Errorf("step %d 'when' has unknown 'op' %q; allowed ops are %v", i, op, worker.GuardOpNames())
	}
	value := ""
	if rv, ok := obj["value"]; ok {
		v, isStr := rv.(string)
		if !isStr {
			return nil, fmt.Errorf("step %d 'when' 'value' must be a string", i)
		}
		value = v
	} else if needsValue {
		return nil, fmt.Errorf("step %d 'when' op %q requires a 'value'", i, op)
	}

	source, _ := obj["source"].(string)
	if source == "" {
		// The default only exists for a single-parent guard. Note DepCount, not
		// len(DepIdx): a step whose one dependency did not RESOLVE (non-strict
		// save) has no parent to read an output key from, and inventing one would
		// bake the wrong key into the flow.
		if st.DepCount != 1 || len(st.DepIdx) != 1 {
			return nil, fmt.Errorf("step %d 'when' must name a 'source' state key: the step has %d "+
				"resolvable dependencies, so there is no single parent to default to", i, len(st.DepIdx))
		}
		dep := st.DepIdx[0]
		depName, _ := steps[dep]["name"].(string)
		if o, ok := steps[dep]["output"].(string); ok && o != "" {
			source = o
		} else {
			source = defaultOutputKey(depName, dep)
		}
	}
	if !produced[source] {
		return nil, fmt.Errorf("step %d 'when' reads state key %q, which no earlier step produces "+
			"(available at this point: %s)", i, source, strings.Join(sortedSet(produced), ", "))
	}
	return &worker.Guard{Source: source, Op: op, Value: value}, nil
}

// registerDAG wires `tag dag save/run/show/list/state`.
func registerDAG(root *cobra.Command, app *App) {
	dag := &cobra.Command{Use: "dag", Short: "DAG workflow engine", GroupID: "orch"}

	var specJSON string
	save := &cobra.Command{Use: "save NAME", Short: "Save a DAG spec", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return jsonErrorMaybe(fmt.Errorf("DAG name must not be empty"))
			}
			var steps []map[string]any
			if err := json.Unmarshal([]byte(specJSON), &steps); err != nil {
				return jsonErrorMaybe(fmt.Errorf("invalid --steps JSON: %w", err))
			}
			// Validate with the SAME builder `dag run` uses, so a saved spec is
			// always an executable one.
			if _, err := buildDagSteps(args[0], steps, "", false); err != nil {
				return jsonErrorMaybe(err)
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			spec, _ := json.Marshal(map[string]any{"name": args[0], "steps": steps})
			_, err = db.Exec(`INSERT INTO queue_dags(id,name,spec_json,created_at) VALUES(?,?,?,?)
				ON CONFLICT(name) DO UPDATE SET spec_json=excluded.spec_json`,
				uuid.NewString()[:12], args[0], string(spec), time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			fmt.Printf("DAG '%s' saved (%d steps)\n", args[0], len(steps))
			return nil
		}}
	save.Flags().StringVar(&specJSON, "steps", "[]", "JSON step array")

	dagList := &cobra.Command{Use: "list", Short: "List DAGs",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT name,spec_json FROM queue_dags ORDER BY name`)
			if err != nil {
				return err
			}
			defer rows.Close()
			items := []map[string]any{}
			for rows.Next() {
				var nm, spec string
				if err := rows.Scan(&nm, &spec); err != nil {
					return err
				}
				var sp map[string]any
				json.Unmarshal([]byte(spec), &sp) //nolint:errcheck // a corrupt row shows 0 steps
				steps, _ := sp["steps"].([]any)
				items = append(items, map[string]any{"name": nm, "steps": len(steps)})
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				b, _ := json.Marshal(items)
				fmt.Println(string(b))
				return nil
			}
			if len(items) == 0 {
				fmt.Println("No DAGs.")
				return nil
			}
			for _, it := range items {
				fmt.Printf("%-30s %d steps\n", it["name"], it["steps"])
			}
			return nil
		}}

	dagShow := &cobra.Command{Use: "show [JOB_ID...]", Short: "Show job dependency graph",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			query := `SELECT id,task,task_type,profile,status,COALESCE(deps_json,'[]'),created_at FROM queue_jobs`
			var qargs []any
			if len(args) > 0 {
				ph := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
				query += ` WHERE id IN (` + ph + `)`
				for _, a := range args {
					qargs = append(qargs, a)
				}
			} else {
				query += ` ORDER BY created_at LIMIT 50`
			}
			rows, err := db.Query(query, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			type jrec struct {
				id, task, ttype, profile, status, deps, created string
			}
			var recs []jrec
			for rows.Next() {
				var r jrec
				if err := rows.Scan(&r.id, &r.task, &r.ttype, &r.profile, &r.status, &r.deps, &r.created); err != nil {
					return err
				}
				recs = append(recs, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			// Explicit args that match nothing must not read as a clean empty
			// result. `dag show` takes JOB IDs; a DAG NAME (what `dag run`/`state`
			// key on) matches none, so `dag show <name>` used to print
			// "No jobs found." / [] with exit 0 — a silent miss. Point at the
			// right command instead.
			if len(args) > 0 && len(recs) == 0 {
				return jsonErrorMaybe(fmt.Errorf("no jobs match %s — `dag show` takes job ids; "+
					"run `dag show` with no args to list jobs, or `dag state <name>` for a DAG run",
					strings.Join(args, ", ")))
			}
			if flagJSON {
				items := []map[string]any{}
				for _, r := range recs {
					var deps []string
					json.Unmarshal([]byte(r.deps), &deps) //nolint:errcheck // a corrupt row shows no deps
					if deps == nil {
						deps = []string{}
					}
					items = append(items, map[string]any{"id": r.id, "task": r.task,
						"task_type": r.ttype, "profile": r.profile, "status": r.status,
						"deps": deps, "created_at": r.created})
				}
				b, _ := json.MarshalIndent(items, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(recs) == 0 {
				fmt.Println("No jobs found.")
				return nil
			}
			icons := map[string]string{"ready": "⏳", "running": "▶", "done": "✓", "failed": "✗",
				"pending": "○", "cancelled": "⊘", "queued": "•", "timed_out": "⌛", "skipped": "–"}
			fmt.Println("Job Dependency Graph")
			fmt.Println(strings.Repeat("=", 40))
			for _, r := range recs {
				icon := icons[r.status]
				if icon == "" {
					icon = "?"
				}
				var deps []string
				json.Unmarshal([]byte(r.deps), &deps) //nolint:errcheck // a corrupt row shows no deps
				depStr := ""
				if len(deps) > 0 {
					short := make([]string, len(deps))
					for i, d := range deps {
						short[i] = truncate(d, 8)
					}
					depStr = " ← [" + strings.Join(short, ", ") + "]"
				}
				fmt.Printf("%s %-12s [%-8s] %s%s\n", icon, truncate(r.id, 12), r.status, truncate(r.task, 50), depStr)
			}
			return nil
		}}

	var runBoard, dagProvider string
	var dagExecute, dagTools bool
	var dagPerms permFlags
	dagRun := &cobra.Command{Use: "run NAME", Short: "Submit a named DAG", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var storedSpec string
			if err := db.QueryRow(`SELECT spec_json FROM queue_dags WHERE name=?`, args[0]).Scan(&storedSpec); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("DAG not found: %q", args[0])
				}
				return err
			}
			var spec struct {
				Steps []map[string]any `json:"steps"`
			}
			if err := json.Unmarshal([]byte(storedSpec), &spec); err != nil {
				return fmt.Errorf("DAG %q has malformed spec: %w", args[0], err)
			}
			// One run id per submission: state is reduced per RUN, so re-running a
			// DAG never mixes its values with the previous run's.
			runID := queueHexID(12)
			built, err := buildDagSteps(args[0], spec.Steps, runID, true)
			if err != nil {
				return err
			}
			if err := worker.EnsureFlowColumn(db.DB); err != nil {
				return err
			}

			now := time.Now().UTC().Format(time.RFC3339)
			var submitted []string
			var ready, pending []string
			for _, st := range built {
				depIDs := make([]string, 0, len(st.DepIdx))
				for _, di := range st.DepIdx {
					depIDs = append(depIDs, submitted[di])
				}
				status := "ready"
				if len(depIDs) > 0 {
					status = "pending"
				}
				id := queueHexID(16)
				profileVal := strOr(st.Profile, "default")
				taskType := strOr(st.TaskType, "mixed")
				depsJSON, _ := json.Marshal(depIDs)
				flowJSON, err := json.Marshal(st.Flow)
				if err != nil {
					return err
				}
				_, err = db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,priority,created_at,notify,deps_json,flow_json)
					VALUES(?,?,?,?,?,5,?,1,?,?)`, id, profileVal,
					strings.ReplaceAll(st.Task, "\x00", ""), taskType, status, now, string(depsJSON), string(flowJSON))
				if err != nil {
					return err
				}
				submitted = append(submitted, id)
				if status == "ready" {
					ready = append(ready, id)
				} else {
					pending = append(pending, id)
				}
			}
			if submitted == nil {
				submitted = []string{}
			}
			if ready == nil {
				ready = []string{}
			}
			if pending == nil {
				pending = []string{}
			}
			// Optional execution: after enqueuing, drain the jobs through the agent
			// loop so `dag run --execute` actually runs work. Default stays
			// enqueue-only (offline parity) unless --execute is given.
			var execSummary *worker.Summary
			if dagExecute {
				opts, err := buildWorkerOptions(app, dagProvider, 0, false, dagTools, &dagPerms)
				if err != nil {
					return err
				}
				opts.OnlyJobs = submitted
				// Cancel on SIGINT/SIGTERM, exactly as `queue worker` does. With a
				// bare context.Background() a SIGTERM killed the process outright and
				// left the in-flight job stranded in 'running' for the full 30-minute
				// staleClaimLease, blocking every dependent job behind it.
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				s, err := worker.Drain(ctx, db.DB, opts)
				if err != nil {
					return err
				}
				execSummary = &s
			}
			if flagJSON {
				payload := map[string]any{"dag": args[0], "run_id": runID, "submitted": submitted,
					"dispatched": ready, "pending": pending}
				if execSummary != nil {
					payload["executed"] = map[string]any{"claimed": execSummary.Claimed, "done": execSummary.Done,
						"failed": execSummary.Failed, "skipped": execSummary.Skipped, "pruned": execSummary.Pruned}
				}
				b, _ := json.Marshal(payload)
				fmt.Println(string(b))
				return nil
			}
			// Offline (no managed runtime): jobs with no unmet deps are marked
			// 'ready'; dependents stay 'pending' until their parents reach 'done'.
			fmt.Printf("DAG '%s' submitted: %d jobs (%d ready, %d pending on dependencies)  run=%s\n",
				args[0], len(submitted), len(ready), len(pending), runID)
			for _, id := range ready {
				fmt.Printf("  %s  (ready)\n", id)
			}
			for _, id := range pending {
				fmt.Printf("  %s  (pending on dependencies)\n", id)
			}
			if execSummary != nil {
				fmt.Printf("executed: %d claimed, %d done, %d failed, %d skipped, %d pruned (branch not taken)\n",
					execSummary.Claimed, execSummary.Done, execSummary.Failed, execSummary.Skipped, execSummary.Pruned)
				// See queue worker: a DAG in which nodes failed must not exit 0.
				if execSummary.Failed > 0 {
					return exitCodeErr{code: exitFindings}
				}
			}
			return nil
		}}
	dagRun.Flags().StringVar(&runBoard, "board", "default", "board")
	dagRun.Flags().BoolVar(&dagExecute, "execute", false, "run enqueued jobs through the agent loop after submitting")
	dagRun.Flags().StringVar(&dagProvider, "provider", "echo", "llm provider for --execute (echo = offline)")
	dagRun.Flags().BoolVar(&dagTools, "tools", false, "enable built-in tools for --execute")
	dagPerms.bind(dagRun)

	dagState := &cobra.Command{Use: "state [RUN_ID]", Short: "Show the reduced state of a DAG run",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			runID := ""
			if len(args) == 1 {
				runID = args[0]
				ok, err := worker.RunExists(ctx, db.DB, runID)
				if err != nil {
					return err
				}
				if !ok {
					// An unknown run must not print an empty state as though that
					// were the answer.
					return jsonErrorMaybe(fmt.Errorf("no DAG run %q (run `tag dag run <name>` to start one)", runID))
				}
			} else {
				runID, err = worker.LatestRunID(ctx, db.DB)
				if err != nil {
					return err
				}
				if runID == "" {
					return jsonErrorMaybe(fmt.Errorf("no DAG run has been submitted yet"))
				}
			}
			state, err := worker.RunState(ctx, db.DB, runID)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			nodes, err := worker.RunNodes(ctx, db.DB, runID)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{"run_id": runID, "state": state, "nodes": nodes})
			}
			fmt.Printf("DAG run %s\n", runID)
			fmt.Println(strings.Repeat("=", 40))
			for _, n := range nodes {
				label := n.Name
				if label == "" {
					label = fmt.Sprintf("step%d", n.Index)
				}
				line := fmt.Sprintf("%-20s [%-8s] -> %s", label, n.Status, n.Output)
				if n.Reason != "" {
					line += "  (" + n.Reason + ")"
				}
				fmt.Println(line)
			}
			if len(state) == 0 {
				fmt.Println("\n(no state: no node has completed yet)")
				return nil
			}
			fmt.Println("\nReduced state:")
			for _, k := range sortedSet(stateKeySet(state)) {
				fmt.Printf("  %s = %s\n", k, truncate(strings.ReplaceAll(state[k], "\n", "\\n"), 100))
			}
			return nil
		}}

	dag.AddCommand(save, dagList, dagShow, dagRun, dagState)
	root.AddCommand(dag)
}

// stateKeySet adapts a state map for sortedSet.
func stateKeySet(state map[string]string) map[string]bool {
	out := make(map[string]bool, len(state))
	for k := range state {
		out[k] = true
	}
	return out
}
