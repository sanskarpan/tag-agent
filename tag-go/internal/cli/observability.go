package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/tag-agent/tag/internal/pricing"
	"github.com/tag-agent/tag/internal/store"

	"github.com/spf13/cobra"
)

// traceSpanRec mirrors the span columns used by the trace snapshot/replay/diff
// commands (port of _build_snapshot's per-span dict).
type traceSpanRec struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Profile    string         `json:"profile"`
	ModelID    string         `json:"model_id"`
	StartedAt  string         `json:"started_at"`
	FinishedAt string         `json:"finished_at"`
	PromptTok  int            `json:"prompt_tokens"`
	CompTok    int            `json:"completion_tokens"`
	Status     string         `json:"status"`
	Attributes map[string]any `json:"attributes"`
	ErrorMsg   any            `json:"error_msg"`
}

// traceBuildSnapshot builds an in-memory snapshot of a trace from live spans
// (read-only; port of _build_snapshot). Returns nil if the trace has no spans.
func traceBuildSnapshot(db *store.DB, traceID string) (map[string]any, error) {
	rows, err := db.Query(`SELECT id,name,COALESCE(profile,''),COALESCE(model_id,''),started_at,COALESCE(finished_at,''),
		prompt_tokens,completion_tokens,status,COALESCE(attributes,'{}'),error_msg FROM spans WHERE trace_id=? ORDER BY started_at`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var spans []traceSpanRec
	for rows.Next() {
		var s traceSpanRec
		var attrs string
		var errMsg *string
		if err := rows.Scan(&s.ID, &s.Name, &s.Profile, &s.ModelID, &s.StartedAt, &s.FinishedAt,
			&s.PromptTok, &s.CompTok, &s.Status, &attrs, &errMsg); err != nil {
			return nil, err
		}
		s.Attributes = map[string]any{}
		json.Unmarshal([]byte(attrs), &s.Attributes)
		if errMsg != nil {
			s.ErrorMsg = *errMsg
		}
		spans = append(spans, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, nil
	}
	spanList := make([]any, len(spans))
	for i, s := range spans {
		spanList[i] = s
	}
	return map[string]any{
		"trace_id":    traceID,
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"spans":       spanList,
	}, nil
}

// traceSnapshotToSpans normalizes a snapshot's "spans" (which may be []any of
// maps after a JSON round-trip, or []any of traceSpanRec fresh) to []map.
func traceSnapshotSpans(snap map[string]any) []map[string]any {
	raw, _ := snap["spans"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, s := range raw {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
			continue
		}
		// traceSpanRec → map via JSON round-trip
		b, _ := json.Marshal(s)
		var m map[string]any
		json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out
}

// traceDiffKey identifies a span within a snapshot for diffing: its name plus
// the 0-based ordinal of that occurrence among same-named spans.
type traceDiffKey struct {
	name    string
	ordinal int
}

// traceDiffIndex keys snapshot spans by (name, ordinal) so repeated span names
// are all retained instead of collapsing onto one another.
func traceDiffIndex(spans []map[string]any) map[traceDiffKey]map[string]any {
	out := map[traceDiffKey]map[string]any{}
	seen := map[string]int{}
	for _, s := range spans {
		n := str(s["name"])
		out[traceDiffKey{name: n, ordinal: seen[n]}] = s
		seen[n]++
	}
	return out
}

func traceDiffDelta(d int) string {
	if d > 0 {
		return fmt.Sprintf("+%d", d)
	}
	return fmt.Sprintf("%d", d)
}

func traceSpanTokens(m map[string]any) int {
	pt, _ := m["prompt_tokens"].(float64)
	ct, _ := m["completion_tokens"].(float64)
	return int(pt) + int(ct)
}

// The embedded pricing table now lives in internal/pricing so the agent loop
// (internal/agent -> internal/trace) can price spans too; internal/cli is not
// importable from there. These aliases keep the cli-local call sites and their
// provenance tests reading exactly as before.
type modelPrice = pricing.Price

var pricingTable = pricing.Table

func lookupPrice(model string) (modelPrice, bool) { return pricing.Lookup(model) }

// costSourceAgg is one population's contribution to `tag costs`. The Go engine
// only ever writes the `runs` table while the Python engine only populates
// tokens/cost on `spans`, so aggregating a single table silently reported zero
// for the other engine's data. Both are reported, separately labelled.
type costSourceAgg struct {
	Source           string  `json:"source"`
	Rows             int64   `json:"rows"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	IncludesEstimate bool    `json:"includes_estimated_rates"`
}

// tableColumns returns the column set of a table (empty if the table is absent),
// so an older DB missing the token/cost columns degrades to an empty section
// instead of failing the whole command.
func tableColumns(db *store.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		cols[n] = true
	}
	return cols, rows.Err()
}

// aggregateCosts rolls up one table into a labelled section. costCol may be "" —
// then every priced row falls back to the pricing table. Rows whose cost is
// derived from an Estimated rate set IncludesEstimate.
func aggregateCosts(db *store.DB, label, table, costCol string) (costSourceAgg, error) {
	agg := costSourceAgg{Source: label}
	cols, err := tableColumns(db, table)
	if err != nil {
		return agg, err
	}
	if !cols["prompt_tokens"] || !cols["completion_tokens"] {
		// Table missing or pre-dates token accounting: report an empty section.
		return agg, nil
	}
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*),COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0) FROM %s`, table),
	).Scan(&agg.Rows, &agg.PromptTokens, &agg.CompletionTokens); err != nil {
		return agg, err
	}
	agg.TotalTokens = agg.PromptTokens + agg.CompletionTokens
	if !cols[costCol] {
		costCol = ""
	}
	// A stored cost is authoritative for that row; only rows lacking one are
	// priced from the embedded table.
	unpriced := "1=1"
	if costCol != "" {
		if err := db.QueryRow(fmt.Sprintf(`SELECT COALESCE(SUM(%s),0) FROM %s`, costCol, table)).Scan(&agg.CostUSD); err != nil {
			return agg, err
		}
		// `runs.estimated_cost_usd` is NOT NULL DEFAULT 0, so an unpopulated row
		// reads as 0 rather than NULL; treat both as "no stored cost".
		unpriced = fmt.Sprintf("(%s IS NULL OR %s = 0)", costCol, costCol)
	}
	if !cols["model_id"] {
		return agg, nil
	}
	rows, err := db.Query(fmt.Sprintf(
		`SELECT COALESCE(model_id,''), COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0)
		 FROM %s WHERE %s AND model_id IS NOT NULL AND model_id <> ''`, table, unpriced))
	if err != nil {
		return agg, err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var p, c int64
		if err := rows.Scan(&model, &p, &c); err != nil {
			return agg, err
		}
		price, ok := lookupPrice(model)
		if !ok {
			continue
		}
		agg.CostUSD += float64(p)/1e6*price.In + float64(c)/1e6*price.Out
		if price.Estimated {
			agg.IncludesEstimate = true
		}
	}
	return agg, rows.Err()
}

func registerObservability(root *cobra.Command, app *App) {
	// costs
	var costsRunID, costsBy string
	costs := &cobra.Command{Use: "costs", Short: "Token usage & cost estimates", GroupID: "obs",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			// PRD-046 §7.4: an itemized single-run report for CI. The default
			// (no --run-id) path below is unchanged — it must keep reporting
			// `runs` and `spans` as separate populations with an overlap warning.
			if costsRunID != "" {
				return jsonErrorMaybe(costsRunReport(db, costsRunID, costsBy))
			}
			// `runs` and `spans` are written by different engines (Go writes
			// runs; Python's tracing writes spans), so aggregating only one of
			// them made `costs` blind to half the data. Report both, labelled.
			runsAgg, err := aggregateCosts(db, "runs", "runs", "estimated_cost_usd")
			if err != nil {
				return err
			}
			spansAgg, err := aggregateCosts(db, "spans", "spans", "cost_usd")
			if err != nil {
				return err
			}
			sources := []string{}
			if runsAgg.Rows > 0 {
				sources = append(sources, "runs")
			}
			if spansAgg.Rows > 0 {
				sources = append(sources, "spans")
			}
			// A run aggregates its spans, so when both populations exist the sum
			// may double-count. That must never be reported silently.
			overlap := runsAgg.Rows > 0 && spansAgg.Rows > 0
			estimated := runsAgg.IncludesEstimate || spansAgg.IncludesEstimate
			totPT := runsAgg.PromptTokens + spansAgg.PromptTokens
			totCT := runsAgg.CompletionTokens + spansAgg.CompletionTokens
			totCost := runsAgg.CostUSD + spansAgg.CostUSD
			if flagJSON {
				b, _ := json.Marshal(map[string]any{
					"by_source": map[string]any{"runs": runsAgg, "spans": spansAgg},
					"totals": map[string]any{
						"prompt_tokens": totPT, "completion_tokens": totCT,
						"total_tokens": totPT + totCT, "cost_usd": totCost,
						"sources": sources, "overlap_warning": overlap,
						"includes_estimated_rates": estimated,
					},
				})
				fmt.Println(string(b))
				return nil
			}
			line := func(label string, p, c int64, cost float64) {
				fmt.Printf("%-6s prompt=%d completion=%d total=%d tokens  cost=$%.6f\n", label, p, c, p+c, cost)
			}
			line("runs", runsAgg.PromptTokens, runsAgg.CompletionTokens, runsAgg.CostUSD)
			line("spans", spansAgg.PromptTokens, spansAgg.CompletionTokens, spansAgg.CostUSD)
			line("TOTAL", totPT, totCT, totCost)
			if overlap {
				fmt.Println("note: runs and spans are different populations (a run aggregates spans); the TOTAL may double-count.")
			}
			if estimated {
				fmt.Println("note: total includes estimated rates that are not authoritative published prices.")
			}
			return nil
		}}
	costs.Flags().StringVar(&costsRunID, "run-id", "", "itemized per-span cost report for a single run")
	costs.Flags().StringVar(&costsBy, "by", "span", "with --run-id: span or model")

	// pricing
	var model string
	var inTok, outTok int
	pricing := &cobra.Command{Use: "pricing", Short: "LLM pricing table", GroupID: "obs"}
	prList := &cobra.Command{Use: "list", Short: "List model prices",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Map iteration order is random, so the human table used to print in a
			// different order on every invocation. Sort for determinism.
			ids := make([]string, 0, len(pricingTable))
			for m := range pricingTable {
				ids = append(ids, m)
			}
			sort.Strings(ids)
			if flagJSON {
				out := map[string]any{}
				for _, m := range ids {
					p := pricingTable[m]
					out[m] = map[string]any{
						"input_usd_per_1m":  p.In,
						"output_usd_per_1m": p.Out,
						"estimated":         p.Estimated,
						"source":            p.Source,
					}
				}
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("%-32s %12s %12s %12s\n", "Model", "In $/1M", "Out $/1M", "")
			for _, m := range ids {
				p := pricingTable[m]
				marker := ""
				if p.Estimated {
					marker = "estimated"
				}
				fmt.Printf("%-32s %12.4f %12.4f %12s\n", m, p.In, p.Out, marker)
			}
			return nil
		}}
	prGet := &cobra.Command{Use: "get", Short: "Compute a cost",
		RunE: func(cmd *cobra.Command, args []string) error {
			if inTok < 0 || outTok < 0 {
				return fmt.Errorf("token counts must be >= 0")
			}
			p, ok := lookupPrice(model)
			if !ok {
				return jsonErrorMaybe(fmt.Errorf("model not found: %q", model))
			}
			cost := float64(inTok)/1e6*p.In + float64(outTok)/1e6*p.Out
			// `pricing get` used to ignore the global --json flag entirely and
			// always print "$%.8f", which no caller could parse.
			if flagJSON {
				b, _ := json.Marshal(map[string]any{
					"model_id": model, "input_tokens": inTok, "output_tokens": outTok,
					"input_usd_per_1m": p.In, "output_usd_per_1m": p.Out,
					"cost_usd": cost, "estimated": p.Estimated, "source": p.Source,
				})
				fmt.Println(string(b))
				return nil
			}
			if p.Estimated {
				fmt.Printf("$%.8f  (estimated — not an authoritative published rate)\n", cost)
				return nil
			}
			fmt.Printf("$%.8f\n", cost)
			return nil
		}}
	prGet.Flags().StringVar(&model, "model", "", "model id")
	prGet.Flags().IntVar(&inTok, "input-tokens", 0, "input tokens")
	prGet.Flags().IntVar(&outTok, "output-tokens", 0, "output tokens")
	// PRD-046 §7.5: inspect the active rate table (and one model's provenance)
	// before trusting any cost figure derived from it.
	var showModel string
	prShow := &cobra.Command{Use: "show", Short: "Inspect the active pricing table", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return jsonErrorMaybe(runPricingShow(showModel))
		}}
	prShow.Flags().StringVar(&showModel, "model", "", "show only this model id")
	pricing.AddCommand(prList, prGet, prShow)

	// trace
	trace := &cobra.Command{Use: "trace", Short: "View trace spans", GroupID: "obs"}
	trList := &cobra.Command{Use: "list", Short: "List recent traces",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// "recent traces" means most-recently-active first. Ordering by the
			// trace_id string put a lexicographically-larger id above a newer one,
			// so the newest trace was not first. Order by each trace's latest span.
			rows, err := db.Query(`SELECT trace_id FROM spans GROUP BY trace_id ORDER BY MAX(started_at) DESC LIMIT 50`)
			if err != nil {
				return err
			}
			defer rows.Close()
			ids := []string{}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				b, _ := json.Marshal(ids)
				fmt.Println(string(b))
			} else if len(ids) == 0 {
				fmt.Println("No spans recorded.")
			} else {
				for _, id := range ids {
					fmt.Println(id)
				}
			}
			return nil
		}}
	// PRD-046 §7.1: `--cost` turns `trace show` into a per-span USD waterfall;
	// `--min-cost-usd` trims a large trace down to the expensive spans.
	var showCost bool
	var showMinCost float64
	trShow := &cobra.Command{Use: "show TRACE_ID", Short: "Show all spans in a trace", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if showMinCost < 0 {
				return jsonErrorMaybe(usageErrorf("--min-cost-usd must be >= 0"))
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			spans, err := loadCostSpans(db, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if len(spans) == 0 {
				// Python parity: an empty ARRAY plus exit 1, never `null`. The
				// status is carried out as an error value rather than os.Exit so
				// deferred cleanup (db handles, temp state) still runs.
				if flagJSON {
					fmt.Println("[]")
				} else {
					fmt.Printf("No spans found for trace %s\n", args[0])
				}
				return exitCodeErr{code: 1}
			}
			// Captured before filtering so a filter that removes every row still
			// names the trace's profile.
			traceProfile := ""
			for _, s := range spans {
				if s.Profile != "" {
					traceProfile = s.Profile
					break
				}
			}
			// Filtering breaks the parent chain, so the nested view is only used
			// when the whole trace is present.
			nested := true
			if cmd.Flags().Changed("min-cost-usd") {
				nested = false
				kept := make([]costSpan, 0, len(spans))
				for _, s := range spans {
					if s.CostUSD != nil && *s.CostUSD >= showMinCost {
						kept = append(kept, s)
					}
				}
				spans = kept
			}
			if flagJSON {
				items := make([]map[string]any, 0, len(spans))
				var totals costTotals
				for _, s := range spans {
					totals.add(s)
					item := costSpanJSON(s)
					item["trace_id"] = args[0]
					items = append(items, item)
				}
				if !showCost {
					// Back-compatible shape: a plain array of spans, now carrying
					// the cost/kind columns the schema already stores (FR-20).
					b, _ := json.MarshalIndent(items, "", "  ")
					fmt.Println(string(b))
					return nil
				}
				return emitJSON(map[string]any{
					"run_id": args[0], "profile": traceProfile, "population": "spans",
					"span_count": totals.Spans, "prompt_tokens": totals.PromptTokens,
					"completion_tokens": totals.CompTokens, "total_cost_usd": totals.CostUSD,
					"unpriced_spans": totals.Unpriced, "includes_estimated_rates": totals.Estimated,
					"spans": items,
				})
			}
			if showCost {
				renderCostWaterfall(args[0], traceProfile, spans, nested)
				return nil
			}
			renderFlameChart(spans, nested)
			return nil
		}}
	trShow.Flags().BoolVar(&showCost, "cost", false, "render the per-span USD cost waterfall")
	trShow.Flags().Float64Var(&showMinCost, "min-cost-usd", 0, "only show spans costing at least this much")

	var exportEndpoint, exportTrace string
	trExport := &cobra.Command{Use: "export", Short: "Export spans as OTLP/JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			q := `SELECT id,trace_id,COALESCE(parent_id,''),name,COALESCE(profile,''),COALESCE(model_id,''),
				started_at,COALESCE(finished_at,''),COALESCE(duration_ms,0),status,prompt_tokens,completion_tokens,cost_usd
				FROM spans`
			var qargs []any
			if exportTrace != "" {
				q += ` WHERE trace_id=? ORDER BY started_at`
				qargs = append(qargs, exportTrace)
			} else {
				q += ` ORDER BY started_at`
			}
			rows, err := db.Query(q, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			var spans []map[string]any
			for rows.Next() {
				var id, tid, pid, name, prof, model, start, fin, status string
				var dur, pt, ct int
				var stored *float64
				if err := rows.Scan(&id, &tid, &pid, &name, &prof, &model, &start, &fin, &dur, &status, &pt, &ct, &stored); err != nil {
					return err
				}
				attrs := []map[string]any{otelAttr("gen_ai.request.model", model), otelAttr("tag.profile", prof),
					otelAttrInt("gen_ai.usage.input_tokens", pt), otelAttrInt("gen_ai.usage.output_tokens", ct)}
				attrs = otelAppendCost(attrs, model, stored, pt, ct)
				spans = append(spans, otelSpanJSON(id, tid, pid, name, start, fin, status, attrs))
			}
			if err := rows.Err(); err != nil {
				return err
			}
			// Live OTLP POST to a collector requires the network export backend,
			// which is out of scope for the offline Go build (same stance as
			// otel-export): emit the OTLP/JSON payload to stdout instead.
			if exportEndpoint != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: offline build does not POST to a collector; emitting OTLP/JSON for %d spans (endpoint %q ignored)\n", len(spans), exportEndpoint)
			}
			b, _ := json.MarshalIndent(otelPayload(spans), "", "  ")
			fmt.Println(string(b))
			return nil
		}}
	trExport.Flags().StringVar(&exportEndpoint, "endpoint", "", "OTLP endpoint (offline: payload emitted to stdout)")
	trExport.Flags().StringVar(&exportTrace, "trace-id", "", "export only this trace")

	trSnapshot := &cobra.Command{Use: "snapshot TRACE_ID", Short: "Capture a snapshot of a trace", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := traceSnapshot(db, args[0]); err != nil {
				return err
			}
			fmt.Printf("Snapshot captured for trace: %s\n", args[0])
			return nil
		}}

	trCheckpoint := &cobra.Command{Use: "checkpoint TRACE_ID", Short: "Snapshot a trace and list its checkpoints", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if err := traceSnapshot(db, args[0]); err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id, created_at FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC`, args[0])
			if err != nil {
				return err
			}
			defer rows.Close()
			type snap struct{ id, created string }
			var snaps []snap
			for rows.Next() {
				var s snap
				if err := rows.Scan(&s.id, &s.created); err != nil {
					return err
				}
				snaps = append(snaps, s)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				items := []map[string]any{}
				for _, s := range snaps {
					items = append(items, map[string]any{"id": s.id, "created_at": s.created})
				}
				b, _ := json.MarshalIndent(items, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("Checkpoints for trace %s:\n", args[0])
			for i, s := range snaps {
				fmt.Printf("  [%d] %s  %s\n", i, s.id, s.created)
			}
			return nil
		}}

	trReplay := &cobra.Command{Use: "replay TRACE_ID", Short: "Replay a trace snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			snap, err := traceLoadSnapshot(db, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if snap == nil {
				// #530: a --json consumer must get a parseable error object on
				// stdout, not just the "error:" line on stderr.
				return jsonErrorMaybe(fmt.Errorf("No snapshot found for trace %s", args[0]))
			}
			spans := traceSnapshotSpans(snap)
			if flagJSON {
				b, _ := json.MarshalIndent(snap, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("Trace replay: %s\n", args[0])
			fmt.Printf("Captured: %v\n", strOr(str(snap["captured_at"]), "?"))
			fmt.Printf("Spans: %d\n\n", len(spans))
			for i, s := range spans {
				fmt.Printf("  [%02d] %-40s %-8s %8d tokens\n", i+1, str(s["name"]), strOr(str(s["status"]), "?"), traceSpanTokens(s))
				if em := str(s["error_msg"]); em != "" {
					fmt.Printf("       error: %s\n", truncate(em, 80))
				}
			}
			return nil
		}}

	trDiff := &cobra.Command{Use: "diff TRACE_A TRACE_B", Short: "Diff two trace snapshots", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			snapA, err := traceLoadSnapshot(db, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if snapA == nil {
				return jsonErrorMaybe(fmt.Errorf("No snapshot for trace %s", args[0]))
			}
			snapB, err := traceLoadSnapshot(db, args[1])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if snapB == nil {
				return jsonErrorMaybe(fmt.Errorf("No snapshot for trace %s", args[1]))
			}
			// A trace normally contains several spans with the same name (e.g.
			// one `llm.call` per turn), so spans are keyed by (name, ordinal
			// within that name) rather than by name alone — keying by name made
			// every repeat overwrite the previous one, dropping its tokens and
			// reporting a bogus delta. Each occurrence is compared positionally
			// against the same occurrence in the other trace, which keeps
			// per-span granularity (and therefore correct totals).
			spansA := traceDiffIndex(traceSnapshotSpans(snapA))
			spansB := traceDiffIndex(traceSnapshotSpans(snapB))
			keySet := map[traceDiffKey]bool{}
			for k := range spansA {
				keySet[k] = true
			}
			for k := range spansB {
				keySet[k] = true
			}
			keys := make([]traceDiffKey, 0, len(keySet))
			for k := range keySet {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if keys[i].name != keys[j].name {
					return keys[i].name < keys[j].name
				}
				return keys[i].ordinal < keys[j].ordinal
			})
			// Occurrence counts decide whether a label needs a #N suffix.
			occurrences := map[string]int{}
			for _, k := range keys {
				if k.ordinal+1 > occurrences[k.name] {
					occurrences[k.name] = k.ordinal + 1
				}
			}
			label := func(k traceDiffKey) string {
				if occurrences[k.name] > 1 {
					return fmt.Sprintf("%s#%d", k.name, k.ordinal+1)
				}
				return k.name
			}
			if flagJSON {
				diff := []map[string]any{}
				for _, k := range keys {
					var a, b any
					if v, ok := spansA[k]; ok {
						a = v
					}
					if v, ok := spansB[k]; ok {
						b = v
					}
					diff = append(diff, map[string]any{"name": k.name, "occurrence": k.ordinal + 1, "a": a, "b": b})
				}
				out, _ := json.MarshalIndent(diff, "", "  ")
				fmt.Println(string(out))
				return nil
			}
			fmt.Printf("Trace diff: %s  vs  %s\n", truncate(args[0], 12), truncate(args[1], 12))
			totalA, totalB := 0, 0
			for _, k := range keys {
				sa, okA := spansA[k]
				sb, okB := spansB[k]
				ta, tb := 0, 0
				staStr, stbStr := "—", "—"
				if okA {
					ta = traceSpanTokens(sa)
					staStr = strOr(str(sa["status"]), "—")
				}
				if okB {
					tb = traceSpanTokens(sb)
					stbStr = strOr(str(sb["status"]), "—")
				}
				totalA += ta
				totalB += tb
				prefix := " "
				if !okA {
					prefix = "+"
				} else if !okB {
					prefix = "-"
				}
				fmt.Printf("%s %-38s %10d %10d %10s %-10s %s\n", prefix, label(k), ta, tb, traceDiffDelta(tb-ta), staStr, stbStr)
			}
			fmt.Printf("%s %-38s %10d %10d %10s\n", " ", "TOTAL", totalA, totalB, traceDiffDelta(totalB-totalA))
			return nil
		}}

	trace.AddCommand(trList, trShow, trExport, trSnapshot, trCheckpoint, trReplay, trDiff)
	root.AddCommand(costs, pricing, trace)
	registerStats(root, app)
}

// traceSnapshot captures a snapshot of a trace into trace_snapshots (port of
// _snapshot_trace: deterministic PK so repeats de-duplicate). No-op if empty.
func traceSnapshot(db *store.DB, traceID string) error {
	snap, err := traceBuildSnapshot(db, traceID)
	if err != nil {
		return err
	}
	if snap == nil {
		return nil
	}
	b, _ := json.Marshal(snap)
	h := sha256.Sum256([]byte(traceID))
	snapID := hex.EncodeToString(h[:])[:16]
	_, err = db.Exec(`INSERT OR REPLACE INTO trace_snapshots(id,trace_id,step_index,snapshot_json,created_at)
		VALUES(?,?,0,?,?)`, snapID, traceID, string(b), str(snap["captured_at"]))
	return err
}

// traceLoadSnapshot returns the latest persisted snapshot for a trace, or —
// when none is stored — an in-memory snapshot built from live spans.
func traceLoadSnapshot(db *store.DB, traceID string) (map[string]any, error) {
	var js string
	err := db.QueryRow(`SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1`, traceID).Scan(&js)
	if err == nil {
		var snap map[string]any
		if json.Unmarshal([]byte(js), &snap) == nil {
			return snap, nil
		}
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	return traceBuildSnapshot(db, traceID)
}
