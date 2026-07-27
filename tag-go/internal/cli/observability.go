package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

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

// pricingTable is the embedded per-1M-token USD cost (gen_ai cost attribution).
// Kept in sync with src/tag/assets/pricing.yaml — in particular it must cover the
// models TAG ships in its own default config (src/tag/config/default.yaml), which
// previously all reported "model not found".
var pricingTable = map[string][2]float64{ // {input, output} $/1M
	"openai/gpt-4o":               {2.5, 10.0},
	"openai/gpt-4o-mini":          {0.15, 0.6},
	"gpt-4o":                      {2.5, 10.0},
	"gpt-4o-mini":                 {0.15, 0.6},
	"anthropic/claude-opus-4-8":   {5.0, 25.0},
	"anthropic/claude-sonnet-4-6": {3.0, 15.0},
	"anthropic/claude-haiku-4-5":  {1.0, 5.0},
	"google/gemini-2.5-pro":       {1.25, 5.0},
	"google/gemini-2.5-flash":     {0.075, 0.3},
	// Models TAG ships as profile defaults.
	"deepseek/deepseek-v4-flash": {0.14, 0.28},
	"deepseek/deepseek-v4-pro":   {0.27, 1.10},
	"deepseek/deepseek-r1":       {0.55, 2.19},
	"qwen/qwen3-coder":           {0.50, 2.00},
	"qwen/qwen-plus":             {0.40, 1.20},
}

// knownProviderPrefixes are the vendor namespaces used in pricingTable keys. A
// bare model alias (e.g. "claude-sonnet-4-6") is the form users type and the form
// run.go treats as distinct from the prefixed id, so resolve it by trying each
// prefix rather than reporting "model not found".
var knownProviderPrefixes = []string{"anthropic/", "openai/", "google/", "deepseek/", "qwen/"}

// lookupPrice resolves a model id to its {input, output} $/1M price, accepting
// either the fully prefixed id or the bare alias.
func lookupPrice(model string) ([2]float64, bool) {
	if p, ok := pricingTable[model]; ok {
		return p, true
	}
	if !strings.Contains(model, "/") {
		for _, prefix := range knownProviderPrefixes {
			if p, ok := pricingTable[prefix+model]; ok {
				return p, true
			}
		}
	}
	return [2]float64{}, false
}

func registerObservability(root *cobra.Command, app *App) {
	// costs
	costs := &cobra.Command{Use: "costs", Short: "Token usage & cost estimates", GroupID: "obs",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var pt, ct int64
			if err := db.QueryRow(`SELECT COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0) FROM spans`).Scan(&pt, &ct); err != nil {
				return err
			}
			// Dollar cost: prefer the persisted cost_usd column, and fall back to
			// the embedded pricing table for spans that recorded a model + tokens
			// but no cost. Previously `costs` printed tokens only, despite having
			// both a pricing table and a cost_usd column available.
			var storedCost float64
			if err := db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM spans`).Scan(&storedCost); err != nil {
				return err
			}
			costUSD := storedCost
			rows, err := db.Query(`SELECT COALESCE(model_id,''), COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0)
				FROM spans WHERE cost_usd IS NULL AND model_id IS NOT NULL AND model_id <> ''`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var model string
				var p, c int64
				if err := rows.Scan(&model, &p, &c); err != nil {
					return err
				}
				if price, ok := lookupPrice(model); ok {
					costUSD += float64(p)/1e6*price[0] + float64(c)/1e6*price[1]
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			out := map[string]any{
				"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct,
				"cost_usd": costUSD,
			}
			if flagJSON {
				b, _ := json.Marshal(map[string]any{"totals": out})
				fmt.Println(string(b))
			} else {
				fmt.Printf("prompt=%d completion=%d total=%d tokens  cost=$%.6f\n", pt, ct, pt+ct, costUSD)
			}
			return nil
		}}
	// pricing
	var model string
	var inTok, outTok int
	pricing := &cobra.Command{Use: "pricing", Short: "LLM pricing table", GroupID: "obs"}
	prList := &cobra.Command{Use: "list", Short: "List model prices",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagJSON {
				b, _ := json.MarshalIndent(pricingTable, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("%-32s %12s %12s\n", "Model", "In $/1M", "Out $/1M")
			for m, p := range pricingTable {
				fmt.Printf("%-32s %12.4f %12.4f\n", m, p[0], p[1])
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
				return fmt.Errorf("model not found: %q", model)
			}
			cost := float64(inTok)/1e6*p[0] + float64(outTok)/1e6*p[1]
			fmt.Printf("$%.8f\n", cost)
			return nil
		}}
	prGet.Flags().StringVar(&model, "model", "", "model id")
	prGet.Flags().IntVar(&inTok, "input-tokens", 0, "input tokens")
	prGet.Flags().IntVar(&outTok, "output-tokens", 0, "output tokens")
	pricing.AddCommand(prList, prGet)

	// trace
	trace := &cobra.Command{Use: "trace", Short: "View trace spans", GroupID: "obs"}
	trList := &cobra.Command{Use: "list", Short: "List recent traces",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT DISTINCT trace_id FROM spans ORDER BY trace_id DESC LIMIT 50`)
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
	trShow := &cobra.Command{Use: "show TRACE_ID", Short: "Show all spans in a trace", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id,trace_id,COALESCE(parent_id,''),name,COALESCE(profile,''),COALESCE(model_id,''),
				started_at,COALESCE(finished_at,''),COALESCE(duration_ms,0),status,prompt_tokens,completion_tokens,COALESCE(attributes,'{}'),error_msg
				FROM spans WHERE trace_id=? ORDER BY started_at`, args[0])
			if err != nil {
				return err
			}
			defer rows.Close()
			type showRec struct {
				id, tid, pid, name, profile, model, start, fin, status, attrs string
				dur, pt, ct                                                   int
				errMsg                                                        *string
			}
			var recs []showRec
			for rows.Next() {
				var r showRec
				if err := rows.Scan(&r.id, &r.tid, &r.pid, &r.name, &r.profile, &r.model, &r.start, &r.fin, &r.dur, &r.status, &r.pt, &r.ct, &r.attrs, &r.errMsg); err != nil {
					return err
				}
				recs = append(recs, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if len(recs) == 0 {
				if flagJSON {
					fmt.Println("[]")
				} else {
					fmt.Printf("No spans found for trace %s\n", args[0])
				}
				os.Exit(1)
			}
			if flagJSON {
				items := make([]map[string]any, 0, len(recs))
				for _, r := range recs {
					var em any
					if r.errMsg != nil {
						em = *r.errMsg
					}
					var at map[string]any
					json.Unmarshal([]byte(r.attrs), &at)
					items = append(items, map[string]any{"id": r.id, "trace_id": r.tid, "parent_id": r.pid,
						"name": r.name, "profile": r.profile, "model_id": r.model, "started_at": r.start,
						"finished_at": r.fin, "duration_ms": r.dur, "status": r.status,
						"prompt_tokens": r.pt, "completion_tokens": r.ct, "attributes": at, "error_msg": em})
				}
				b, _ := json.MarshalIndent(items, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			for _, r := range recs {
				fmt.Printf("  %-40s %-8s %dms\n", r.name, r.status, r.dur)
			}
			return nil
		}}

	var exportEndpoint, exportTrace string
	trExport := &cobra.Command{Use: "export", Short: "Export spans as OTLP/JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			q := `SELECT id,trace_id,COALESCE(parent_id,''),name,COALESCE(profile,''),COALESCE(model_id,''),
				started_at,COALESCE(finished_at,''),COALESCE(duration_ms,0),status,prompt_tokens,completion_tokens
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
				if err := rows.Scan(&id, &tid, &pid, &name, &prof, &model, &start, &fin, &dur, &status, &pt, &ct); err != nil {
					return err
				}
				attrs := []map[string]any{otelAttr("gen_ai.request.model", model), otelAttr("tag.profile", prof),
					otelAttrInt("gen_ai.usage.input_tokens", pt), otelAttrInt("gen_ai.usage.output_tokens", ct)}
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
				return err
			}
			if snap == nil {
				return fmt.Errorf("No snapshot found for trace %s", args[0])
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
				return err
			}
			if snapA == nil {
				return fmt.Errorf("No snapshot for trace %s", args[0])
			}
			snapB, err := traceLoadSnapshot(db, args[1])
			if err != nil {
				return err
			}
			if snapB == nil {
				return fmt.Errorf("No snapshot for trace %s", args[1])
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
