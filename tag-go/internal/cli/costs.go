package cli

// Per-span USD cost attribution (PRD-046) and the flame-chart renderer that
// `trace show` uses (PRD-013 §6.4 / G3).
//
// Three rules govern everything in this file:
//
//  1. Rates come from internal/pricing. They are never re-derived here — the
//     table moved out of internal/cli precisely so the agent loop could price
//     spans at close time, and two copies would drift.
//  2. Provenance survives aggregation. A pricing row carries Estimated/Source;
//     any figure that depends on an estimated rate is marked (`~` in the tables,
//     `includes_estimated_rates` / `cost_rate_estimated` in JSON) so an estimate
//     is never laundered into an exact-looking number.
//  3. A model with no known rate yields a NULL cost, is rendered as an em dash,
//     and is EXCLUDED from every total — with a one-line summary warning so the
//     omission is visible (PRD-046 G10/NFR-07). It never becomes $0.
//
// The aggregation here reads the `spans` population only. `tag costs` (no
// --run-id) keeps reporting `runs` and `spans` as separately labelled
// populations with an overlap warning, because a run aggregates its spans and
// summing the two double-counts (#586); nothing in this file changes that.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/pricing"
	"github.com/tag-agent/tag/internal/store"
)

// decodeAttrs parses a span's attributes blob, degrading to an empty object
// rather than failing the whole command on a malformed row.
func decodeAttrs(blob string) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal([]byte(blob), &m)
	return m
}

// emDash is the "no known value" cell used by every cost table.
const emDash = "—"

// costSpan is a span row enriched with a resolved cost and the provenance of the
// rate that produced it.
type costSpan struct {
	ID, ParentID, Name, Kind     string
	Profile, ModelID, Status     string
	StartedAt, FinishedAt        string
	DurationMS                   int
	PromptTokens, CompTokens     int
	Attributes                   map[string]any
	ErrorMsg                     *string
	CostUSD                      *float64
	RateEstimated, RateAvailable bool
}

// resolveSpanCost turns a stored cost (which may be NULL) plus the span's model
// and token counts into a cost and its provenance.
//
// A stored cost is authoritative for that row — the agent loop already priced it
// at close time — but the provenance flag still comes from the pricing table,
// because the stored number was derived from that same rate. When nothing was
// stored the row is priced on the fly, which is what makes spans written by the
// Python engine (and pre-#590 rows) reportable.
func resolveSpanCost(modelID string, stored *float64, promptTokens, completionTokens int) (cost *float64, estimated, rateKnown bool) {
	price, ok := pricing.Lookup(modelID)
	if stored != nil {
		return stored, ok && price.Estimated, true
	}
	if !ok || (promptTokens == 0 && completionTokens == 0) {
		return nil, false, ok
	}
	c := float64(promptTokens)/1e6*price.In + float64(completionTokens)/1e6*price.Out
	return &c, price.Estimated, true
}

// loadCostSpans reads every span of a trace, ordered by start time, with costs
// resolved. An unknown trace yields an empty slice (never nil).
func loadCostSpans(db *store.DB, traceID string) ([]costSpan, error) {
	rows, err := db.Query(`SELECT id, trace_id, COALESCE(parent_id,''), name, COALESCE(kind,''),
		COALESCE(profile,''), COALESCE(model_id,''), started_at, COALESCE(finished_at,''),
		COALESCE(duration_ms,0), status, prompt_tokens, completion_tokens,
		COALESCE(attributes,'{}'), error_msg, cost_usd
		FROM spans WHERE trace_id=? ORDER BY started_at`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []costSpan{}
	for rows.Next() {
		var s costSpan
		var tid, attrs string
		var stored *float64
		if err := rows.Scan(&s.ID, &tid, &s.ParentID, &s.Name, &s.Kind, &s.Profile, &s.ModelID,
			&s.StartedAt, &s.FinishedAt, &s.DurationMS, &s.Status, &s.PromptTokens, &s.CompTokens,
			&attrs, &s.ErrorMsg, &stored); err != nil {
			return nil, err
		}
		s.Attributes = decodeAttrs(attrs)
		s.CostUSD, s.RateEstimated, s.RateAvailable = resolveSpanCost(s.ModelID, stored, s.PromptTokens, s.CompTokens)
		out = append(out, s)
	}
	return out, rows.Err()
}

// costTotals is the roll-up shared by every cost surface.
type costTotals struct {
	Spans        int
	PromptTokens int
	CompTokens   int
	CostUSD      float64
	Unpriced     int
	Estimated    bool
}

func (t *costTotals) add(s costSpan) {
	t.Spans++
	t.PromptTokens += s.PromptTokens
	t.CompTokens += s.CompTokens
	if s.CostUSD == nil {
		// "Unpriced" means TAG has no RATE for the span's model — that is the
		// omission a reader must be warned about. A span with no model at all (a
		// root agent.run, a tool call) or one whose model IS priced but recorded
		// zero tokens is simply not billable, and counting either here would
		// raise a cost warning about spans that were never missing a rate.
		if s.ModelID != "" && !s.RateAvailable {
			t.Unpriced++
		}
		return
	}
	t.CostUSD += *s.CostUSD
	if s.RateEstimated {
		t.Estimated = true
	}
}

// costCell formats a cost for a table: an em dash when unknown, and a trailing
// `~` when the rate behind it is an estimate.
func costCell(cost *float64, estimated bool) string {
	if cost == nil {
		return emDash
	}
	if estimated {
		return fmt.Sprintf("$%.6f~", *cost)
	}
	return fmt.Sprintf("$%.6f", *cost)
}

func intCell(n int) string {
	if n == 0 {
		return emDash
	}
	return fmt.Sprintf("%d", n)
}

func strCell(s string) string {
	if s == "" {
		return emDash
	}
	return s
}

// printCostNotes emits the provenance/omission warnings that must accompany any
// total. Both are single summary lines, not per-span spam (NFR-07).
func printCostNotes(t costTotals) {
	if t.Unpriced > 0 {
		fmt.Printf("note: %d span(s) have no known rate for their model, render as %s, and are excluded from the TOTAL.\n",
			t.Unpriced, emDash)
	}
	if t.Estimated {
		fmt.Printf("note: ~ marks a cost derived from an estimated rate that is not an authoritative published price.\n")
	}
}

// ---------------------------------------------------------------------------
// Tree ordering / flame chart (PRD-013 G3)
// ---------------------------------------------------------------------------

// orderSpanTree returns the spans in depth-first parent→child order together
// with each one's nesting depth. Spans whose parent is missing from the set are
// treated as roots so an orphan is still rendered, and a visited set makes a
// cyclic parent chain terminate instead of recursing forever.
func orderSpanTree(spans []costSpan) ([]costSpan, []int) {
	children := map[string][]int{}
	present := map[string]bool{}
	for i, s := range spans {
		present[s.ID] = true
		_ = i
	}
	var rootIdx []int
	for i, s := range spans {
		if s.ParentID == "" || !present[s.ParentID] || s.ParentID == s.ID {
			rootIdx = append(rootIdx, i)
			continue
		}
		children[s.ParentID] = append(children[s.ParentID], i)
	}
	ordered := make([]costSpan, 0, len(spans))
	depths := make([]int, 0, len(spans))
	visited := map[int]bool{}
	var walk func(i, depth int)
	walk = func(i, depth int) {
		if visited[i] {
			return
		}
		visited[i] = true
		ordered = append(ordered, spans[i])
		depths = append(depths, depth)
		for _, c := range children[spans[i].ID] {
			walk(c, depth+1)
		}
	}
	for _, i := range rootIdx {
		walk(i, 0)
	}
	// Anything left over (only reachable through a cycle) is appended flat so no
	// span is silently dropped from the view.
	for i := range spans {
		if !visited[i] {
			visited[i] = true
			ordered = append(ordered, spans[i])
			depths = append(depths, 0)
		}
	}
	return ordered, depths
}

// spanStatusIcon mirrors tracing.render_trace_terminal's plain-text glyphs.
func spanStatusIcon(status string) string {
	switch status {
	case "ok":
		return "✓"
	case "timeout":
		return "⚠"
	default:
		return "✗"
	}
}

// durationBar renders a proportional flame-chart bar (20 cells).
func durationBar(ms, maxMS int) string {
	const width = 20
	if maxMS <= 0 {
		maxMS = 1
	}
	filled := int(float64(ms) / float64(maxMS) * width)
	if filled < 1 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// renderFlameChart prints the nested, bar-annotated span tree that PRD-013 G3
// asks for. The Go renderer used to print a flat list, so a trace's shape (which
// tool call ran under which turn) was invisible even though parent_id was
// recorded on every span.
func renderFlameChart(spans []costSpan, nested bool) {
	ordered, depths := spans, make([]int, len(spans))
	if nested {
		ordered, depths = orderSpanTree(spans)
	}
	maxMS := 1
	for _, s := range ordered {
		if s.DurationMS > maxMS {
			maxMS = s.DurationMS
		}
	}
	for i, s := range ordered {
		fmt.Printf("%s%s %s  %s  %dms  %d↑%d↓\n",
			strings.Repeat("  ", depths[i]), spanStatusIcon(s.Status), s.Name,
			durationBar(s.DurationMS, maxMS), s.DurationMS, s.PromptTokens, s.CompTokens)
		if s.ErrorMsg != nil && *s.ErrorMsg != "" {
			fmt.Printf("%s  error: %s\n", strings.Repeat("  ", depths[i]), truncate(*s.ErrorMsg, 90))
		}
	}
}

// renderCostWaterfall prints the per-span cost view of PRD-046 §7.1. profile is
// the trace's profile taken BEFORE any --min-cost-usd filtering, so a filter that
// removes every row still names the trace it filtered.
func renderCostWaterfall(traceID, profile string, spans []costSpan, nested bool) {
	ordered, depths := spans, make([]int, len(spans))
	if nested {
		ordered, depths = orderSpanTree(spans)
	}
	fmt.Printf("Trace: %s  profile: %s\n", traceID, strCell(profile))
	rule := strings.Repeat("─", 96)
	fmt.Println(rule)
	fmt.Printf("%-40s %-24s %10s %10s %16s\n", "SPAN", "MODEL", "IN TOK", "OUT TOK", "COST USD")
	fmt.Println(rule)
	var totals costTotals
	for i, s := range ordered {
		totals.add(s)
		name := strings.Repeat("  ", depths[i]) + s.Name
		fmt.Printf("%-40s %-24s %10s %10s %16s\n", truncate(name, 40), truncate(strCell(s.ModelID), 24),
			intCell(s.PromptTokens), intCell(s.CompTokens), costCell(s.CostUSD, s.RateEstimated))
	}
	fmt.Println(rule)
	total := totals.CostUSD
	fmt.Printf("%-40s %-24s %10d %10d %16s\n", "TOTAL", "", totals.PromptTokens, totals.CompTokens,
		costCell(&total, totals.Estimated))
	fmt.Println(rule)
	printCostNotes(totals)
}

// costSpanJSON is the machine-readable shape of one span in every cost surface.
func costSpanJSON(s costSpan) map[string]any {
	var em any
	if s.ErrorMsg != nil {
		em = *s.ErrorMsg
	}
	var cost any
	if s.CostUSD != nil {
		cost = *s.CostUSD
	}
	return map[string]any{
		"id": s.ID, "parent_id": s.ParentID, "name": s.Name, "kind": s.Kind,
		"profile": s.Profile, "model_id": s.ModelID, "started_at": s.StartedAt,
		"finished_at": s.FinishedAt, "duration_ms": s.DurationMS, "status": s.Status,
		"prompt_tokens": s.PromptTokens, "completion_tokens": s.CompTokens,
		"attributes": s.Attributes, "error_msg": em,
		"cost_usd": cost, "cost_rate_estimated": s.RateEstimated,
	}
}

// ---------------------------------------------------------------------------
// costs --run-id  (PRD-046 §7.4)
// ---------------------------------------------------------------------------

// costsRunReport implements the itemized single-run report. The unknown-run case
// is a runtime error (exit 1) rather than an empty success, so a CI pipeline
// asserting on a run's cost cannot pass because it typo'd the id.
func costsRunReport(db *store.DB, runID, by string) error {
	if by != "span" && by != "model" {
		return usageErrorf("invalid --by %q; expected span or model", by)
	}
	spans, err := loadCostSpans(db, runID)
	if err != nil {
		return err
	}
	if len(spans) == 0 {
		return fmt.Errorf("no spans recorded for run %q", runID)
	}
	profile := ""
	started := ""
	for _, s := range spans {
		if profile == "" {
			profile = s.Profile
		}
		if started == "" {
			started = s.StartedAt
		}
	}
	var totals costTotals
	for _, s := range spans {
		totals.add(s)
	}

	type row struct {
		label     string
		model     string
		pt, ct    int
		cost      *float64
		estimated bool
		count     int
	}
	var rows []row
	if by == "span" {
		for _, s := range spans {
			rows = append(rows, row{label: s.Name, model: s.ModelID, pt: s.PromptTokens,
				ct: s.CompTokens, cost: s.CostUSD, estimated: s.RateEstimated, count: 1})
		}
	} else {
		order := []string{}
		agg := map[string]*row{}
		for _, s := range spans {
			key := s.ModelID
			r, ok := agg[key]
			if !ok {
				r = &row{label: key, model: key}
				agg[key] = r
				order = append(order, key)
			}
			r.count++
			r.pt += s.PromptTokens
			r.ct += s.CompTokens
			if s.CostUSD != nil {
				c := 0.0
				if r.cost != nil {
					c = *r.cost
				}
				c += *s.CostUSD
				r.cost = &c
				r.estimated = r.estimated || s.RateEstimated
			}
		}
		for _, k := range order {
			rows = append(rows, *agg[k])
		}
	}

	if flagJSON {
		items := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			var cost any
			if r.cost != nil {
				cost = *r.cost
			}
			item := map[string]any{
				"model_id": r.model, "prompt_tokens": r.pt, "completion_tokens": r.ct,
				"total_tokens": r.pt + r.ct, "cost_usd": cost, "cost_rate_estimated": r.estimated,
			}
			if by == "span" {
				item["span_name"] = r.label
			} else {
				item["span_count"] = r.count
			}
			items = append(items, item)
		}
		return emitJSON(map[string]any{
			"run_id": runID, "profile": profile, "started_at": started, "by": by,
			"span_count": totals.Spans, "prompt_tokens": totals.PromptTokens,
			"completion_tokens": totals.CompTokens, "total_cost_usd": totals.CostUSD,
			"unpriced_spans": totals.Unpriced, "includes_estimated_rates": totals.Estimated,
			"population": "spans", "rows": items,
		})
	}

	fmt.Printf("Run: %s  profile: %s  started: %s\n", runID, strCell(profile), strCell(started))
	rule := strings.Repeat("─", 86)
	fmt.Println(rule)
	// Aggregating by model makes the span-name column meaningless, and repeating
	// the model in two adjacent columns just reads as a bug — so the layout
	// changes with the mode instead.
	if by == "model" {
		fmt.Printf("%-30s %8s %14s %14s %16s\n", "MODEL", "SPANS", "IN TOK", "OUT TOK", "COST USD")
		fmt.Println(rule)
		for _, r := range rows {
			fmt.Printf("%-30s %8d %14s %14s %16s\n", truncate(strCell(r.model), 30), r.count,
				intCell(r.pt), intCell(r.ct), costCell(r.cost, r.estimated))
		}
		fmt.Println(rule)
		total := totals.CostUSD
		fmt.Printf("%-30s %8d %14d %14d %16s\n", "TOTAL", totals.Spans, totals.PromptTokens,
			totals.CompTokens, costCell(&total, totals.Estimated))
	} else {
		fmt.Printf("%-30s %-24s %10s %10s %16s\n", "SPAN NAME", "MODEL", "IN TOK", "OUT TOK", "COST USD")
		fmt.Println(rule)
		for _, r := range rows {
			fmt.Printf("%-30s %-24s %10s %10s %16s\n", truncate(r.label, 30), truncate(strCell(r.model), 24),
				intCell(r.pt), intCell(r.ct), costCell(r.cost, r.estimated))
		}
		fmt.Println(rule)
		total := totals.CostUSD
		fmt.Printf("%-30s %-24s %10d %10d %16s\n", "TOTAL", "", totals.PromptTokens, totals.CompTokens,
			costCell(&total, totals.Estimated))
	}
	fmt.Println(rule)
	printCostNotes(totals)
	fmt.Println("population: spans (a run aggregates its spans; `tag costs` without --run-id reports both populations).")
	return nil
}

// ---------------------------------------------------------------------------
// stats --cost  (PRD-046 §7.2 / G5)
// ---------------------------------------------------------------------------

var statsByFields = map[string]bool{"model": true, "profile": true, "day": true, "week": true}

// parseCostWindow accepts either a relative window ("7d"/"2w"/"1m") or an
// absolute ISO date/timestamp, and returns a comparable ISO prefix. Span
// timestamps are ISO-8601 UTC strings, so lexical comparison IS chronological.
func parseCostWindow(v string, endOfDay bool) (string, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", fmt.Errorf("empty time bound")
	}
	last := s[len(s)-1]
	if last == 'd' || last == 'w' || last == 'm' || last == 'D' || last == 'W' || last == 'M' {
		return parseSince(s)
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if endOfDay {
			return t.Format("2006-01-02") + "T23:59:59", nil
		}
		return t.Format("2006-01-02") + "T00:00:00", nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05"), nil
	}
	return "", fmt.Errorf("invalid time value %q; expected 7d/2w/1m, YYYY-MM-DD, or an RFC3339 timestamp", v)
}

// isoWeekKey renders an ISO year-week bucket, falling back to the raw date when
// the timestamp cannot be parsed (a span written by an older engine).
func isoWeekKey(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		if t, err = time.Parse("2006-01-02T15:04:05", iso); err != nil {
			if len(iso) >= 10 {
				return iso[:10]
			}
			return iso
		}
	}
	y, w := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

type statsGroup struct {
	Key    string
	Fields map[string]string
	Spans  int
	PT, CT int
	Cost   float64
	// Priced counts the spans that actually contributed a cost. When it is zero
	// the group's cost is UNKNOWN, not $0.00 — reporting a hard zero for a group
	// TAG could not price is the same laundering the em-dash rule exists to stop.
	Priced    int
	Unpriced  int
	Estimated bool
}

// cost returns the group's cost, or nil when nothing in it could be priced.
func (g *statsGroup) cost() *float64 {
	if g.Priced == 0 {
		return nil
	}
	c := g.Cost
	return &c
}

func runStatsCost(db *store.DB, byFields []string, since, until, profile string, minCost float64) error {
	sinceISO, err := parseCostWindow(since, false)
	if err != nil {
		return usageErrorf("--since: %v", err)
	}
	untilISO := time.Now().UTC().Format("2006-01-02T15:04:05")
	if until != "" {
		if untilISO, err = parseCostWindow(until, true); err != nil {
			return usageErrorf("--until: %v", err)
		}
	}
	// Span timestamps are RFC3339 with a fractional part and a trailing Z
	// ("2026-08-01T05:20:16.121041Z") while the window bounds are second-precision
	// ISO strings. Comparing the raw strings therefore drops every span written in
	// the SAME SECOND as `until` — including, for a default `until = now`, the run
	// the user just made. Both bounds are compared on the second-precision prefix
	// so the window is inclusive at exactly the resolution it is expressed in.
	q := `SELECT COALESCE(model_id,''), COALESCE(profile,''), started_at, prompt_tokens,
		completion_tokens, cost_usd FROM spans
		WHERE substr(started_at,1,19) >= ? AND substr(started_at,1,19) <= ?`
	args := []any{sinceISO, untilISO}
	if profile != "" {
		q += ` AND profile = ?`
		args = append(args, profile)
	}
	q += ` ORDER BY started_at`
	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	groups := map[string]*statsGroup{}
	order := []string{}
	var totals costTotals
	for rows.Next() {
		var model, prof, started string
		var pt, ct int
		var stored *float64
		if err := rows.Scan(&model, &prof, &started, &pt, &ct, &stored); err != nil {
			return err
		}
		cost, estimated, rateKnown := resolveSpanCost(model, stored, pt, ct)
		totals.add(costSpan{ModelID: model, PromptTokens: pt, CompTokens: ct,
			CostUSD: cost, RateEstimated: estimated, RateAvailable: rateKnown})

		fields := map[string]string{}
		parts := make([]string, 0, len(byFields))
		for _, f := range byFields {
			var v string
			switch f {
			case "model":
				v = model
				fields["model_id"] = model
			case "profile":
				v = prof
				fields["profile"] = prof
			case "day":
				if len(started) >= 10 {
					v = started[:10]
				} else {
					v = started
				}
				fields["day"] = v
			case "week":
				v = isoWeekKey(started)
				fields["week"] = v
			}
			parts = append(parts, v)
		}
		key := strings.Join(parts, "\x00")
		g, ok := groups[key]
		if !ok {
			g = &statsGroup{Key: key, Fields: fields}
			groups[key] = g
			order = append(order, key)
		}
		g.Spans++
		g.PT += pt
		g.CT += ct
		if cost == nil {
			if model != "" && !rateKnown {
				g.Unpriced++
			}
		} else {
			g.Priced++
			g.Cost += *cost
			g.Estimated = g.Estimated || estimated
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	kept := make([]*statsGroup, 0, len(order))
	for _, k := range order {
		if g := groups[k]; g.Cost >= minCost {
			kept = append(kept, g)
		}
	}
	// Most expensive first; ties broken by key so the table is deterministic.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Cost != kept[j].Cost {
			return kept[i].Cost > kept[j].Cost
		}
		return kept[i].Key < kept[j].Key
	})

	if flagJSON {
		items := make([]map[string]any, 0, len(kept))
		for _, g := range kept {
			var cost any
			if c := g.cost(); c != nil {
				cost = *c
			}
			item := map[string]any{
				"span_count": g.Spans, "input_tokens": g.PT, "output_tokens": g.CT,
				"cost_usd": cost, "unpriced_spans": g.Unpriced,
				"includes_estimated_rates": g.Estimated,
			}
			for k, v := range g.Fields {
				item[k] = v
			}
			items = append(items, item)
		}
		return emitJSON(map[string]any{
			"since": sinceISO, "until": untilISO, "by": byFields, "profile": profile,
			"population": "spans", "span_count": totals.Spans,
			"input_tokens": totals.PromptTokens, "output_tokens": totals.CompTokens,
			"total_cost_usd": totals.CostUSD, "unpriced_spans": totals.Unpriced,
			"includes_estimated_rates": totals.Estimated, "groups": items,
		})
	}

	fmt.Printf("Cost Summary  (since %s, until %s, grouped by %s)\n", sinceISO, untilISO, strings.Join(byFields, "+"))
	rule := strings.Repeat("─", 84)
	fmt.Println(rule)
	label := strings.ToUpper(strings.Join(byFields, " / "))
	fmt.Printf("%-34s %8s %12s %12s %14s\n", label, "SPANS", "IN TOK", "OUT TOK", "TOTAL USD")
	fmt.Println(rule)
	for _, g := range kept {
		parts := make([]string, 0, len(byFields))
		for _, f := range byFields {
			switch f {
			case "model":
				parts = append(parts, strCell(g.Fields["model_id"]))
			case "profile":
				parts = append(parts, strCell(g.Fields["profile"]))
			default:
				parts = append(parts, strCell(g.Fields[f]))
			}
		}
		fmt.Printf("%-34s %8d %12d %12d %14s\n", truncate(strings.Join(parts, " / "), 34),
			g.Spans, g.PT, g.CT, costCell(g.cost(), g.Estimated))
	}
	fmt.Println(rule)
	total := totals.CostUSD
	fmt.Printf("%-34s %8d %12d %12d %14s\n", "TOTAL", totals.Spans, totals.PromptTokens, totals.CompTokens,
		costCell(&total, totals.Estimated))
	fmt.Println(rule)
	printCostNotes(totals)
	fmt.Println("population: spans (a run aggregates its spans; `tag costs` reports runs and spans separately).")
	return nil
}

// registerStats wires `tag stats --cost`. It lives here rather than in root.go so
// the observability surface stays self-contained.
func registerStats(parent *cobra.Command, app *App) {
	var (
		cost    bool
		since   string
		until   string
		by      []string
		profile string
		minCost float64
	)
	c := &cobra.Command{
		Use:     "stats",
		Short:   "Aggregate per-span USD cost across runs (--cost)",
		GroupID: "obs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cost {
				return jsonErrorMaybe(usageErrorf("stats requires --cost (cost aggregation is the only supported mode)"))
			}
			if minCost < 0 {
				return jsonErrorMaybe(usageErrorf("--min-cost-usd must be >= 0"))
			}
			fields := by
			if len(fields) == 0 {
				fields = []string{"model"}
			}
			for _, f := range fields {
				if !statsByFields[f] {
					return jsonErrorMaybe(usageErrorf("invalid --by %q; expected model, profile, day, or week", f))
				}
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			return jsonErrorMaybe(runStatsCost(db, fields, since, until, profile, minCost))
		},
	}
	c.Flags().BoolVar(&cost, "cost", false, "activate cost aggregation (required)")
	c.Flags().StringVar(&since, "since", "30d", "window start: 7d/2w/1m, YYYY-MM-DD, or RFC3339")
	c.Flags().StringVar(&until, "until", "", "window end: YYYY-MM-DD or RFC3339 (default: now)")
	c.Flags().StringArrayVar(&by, "by", nil, "group by model|profile|day|week (repeatable)")
	c.Flags().StringVar(&profile, "profile", "", "only spans from this profile")
	c.Flags().Float64Var(&minCost, "min-cost-usd", 0, "drop groups below this cost")
	parent.AddCommand(c)
}

// ---------------------------------------------------------------------------
// pricing show  (PRD-046 §7.5)
// ---------------------------------------------------------------------------

func runPricingShow(model string) error {
	if model != "" {
		p, ok := pricing.Lookup(model)
		if !ok {
			return fmt.Errorf("model not found in the pricing table: %q", model)
		}
		if flagJSON {
			return emitJSON(map[string]any{
				"model_id": model, "input_usd_per_1m": p.In, "output_usd_per_1m": p.Out,
				"estimated": p.Estimated, "source": p.Source,
			})
		}
		marker := ""
		if p.Estimated {
			marker = "  (estimated — NOT an authoritative published rate)"
		}
		fmt.Printf("%-32s in $%.4f/1M  out $%.4f/1M%s\n", model, p.In, p.Out, marker)
		fmt.Printf("source: %s\n", strCell(p.Source))
		return nil
	}
	ids := make([]string, 0, len(pricing.Table))
	for m := range pricing.Table {
		ids = append(ids, m)
	}
	sort.Strings(ids)
	if flagJSON {
		items := make([]map[string]any, 0, len(ids))
		for _, m := range ids {
			p := pricing.Table[m]
			items = append(items, map[string]any{
				"model_id": m, "input_usd_per_1m": p.In, "output_usd_per_1m": p.Out,
				"estimated": p.Estimated, "source": p.Source,
			})
		}
		return emitJSON(map[string]any{"models": items, "count": len(items)})
	}
	fmt.Println("Active pricing table  (built-in, internal/pricing)")
	rule := strings.Repeat("─", 100)
	fmt.Println(rule)
	fmt.Printf("%-30s %14s %14s  %s\n", "MODEL ID", "IN $/1M TOK", "OUT $/1M TOK", "SOURCE")
	fmt.Println(rule)
	estimated := 0
	for _, m := range ids {
		p := pricing.Table[m]
		src := strCell(p.Source)
		if p.Estimated {
			estimated++
			src = "~ " + src
		}
		fmt.Printf("%-30s %14.4f %14.4f  %s\n", m, p.In, p.Out, truncate(src, 48))
	}
	fmt.Println(rule)
	fmt.Printf("(%d models total", len(ids))
	if estimated > 0 {
		fmt.Printf("; %d marked ~ are estimated and NOT authoritative published rates", estimated)
	}
	fmt.Println(")")
	return nil
}
