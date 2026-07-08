package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// cacheInputRates mirrors the "prompt" column of src/tag/cmd/observability.py's
// _COST_TABLE (USD per 1k input tokens). Unknown models fall back to 0.003.
var cacheInputRates = map[string]float64{
	"openai/gpt-4o":                     0.005,
	"openai/gpt-4o-mini":                0.00015,
	"openai/gpt-4-turbo":                0.01,
	"openai/gpt-3.5-turbo":              0.0005,
	"anthropic/claude-sonnet-4-6":       0.003,
	"anthropic/claude-opus-4-8":         0.015,
	"anthropic/claude-haiku-4-5":        0.00025,
	"google/gemini-2.5-pro":             0.00125,
	"google/gemini-2.5-flash":           0.000075,
	"meta-llama/llama-3.3-70b-instruct": 0.00059,
}

// cacheSavings ports _cache_savings: returns (savings, write_premium, net).
func cacheSavings(cacheRead, cacheCreate int, modelID string) (float64, float64, float64) {
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheCreate < 0 {
		cacheCreate = 0
	}
	inputRate := 0.003
	if r, ok := cacheInputRates[modelID]; ok {
		inputRate = r
	}
	savings := (float64(cacheRead) / 1000.0) * inputRate * 0.9
	writeMult := 1.25
	if strings.Contains(strings.ToLower(modelID), "haiku") {
		writeMult = 2.0
	}
	writePremium := (float64(cacheCreate) / 1000.0) * inputRate * (writeMult - 1.0)
	return savings, writePremium, savings - writePremium
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }
func round6(f float64) float64 { return math.Round(f*1000000) / 1000000 }

// parseSinceDays converts "7d"/"2w"/"1m" to a day count (mirrors
// _parse_since_delta().days with a floor of 1).
func parseSinceDays(since string) (int, error) {
	s := strings.ToLower(strings.TrimSpace(since))
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid --since value; expected e.g. 7d, 2w, 1m")
	}
	numPart := s[:len(s)-1]
	n, err := strconv.Atoi(numPart)
	if err != nil || n < 0 || strings.HasPrefix(numPart, "-") || strings.HasPrefix(numPart, "+") {
		return 0, fmt.Errorf("invalid --since value; expected e.g. 7d, 2w, 1m")
	}
	switch s[len(s)-1] {
	case 'd':
		// n days
	case 'w':
		n *= 7
	case 'm':
		n *= 30
	default:
		return 0, fmt.Errorf("invalid --since unit; expected d, w, or m")
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}

// registerCache wires prompt-cache analytics: cache stats.
// Port of src/tag/cmd/observability.py:cmd_cache (stats). Reads the runs table's
// cache/prompt/completion token columns (populated by `tag run`).
func registerCache(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "cache", Short: "Prompt cache analytics", GroupID: "obs"}

	var profile, model, since string
	var warnThreshold float64
	stats := &cobra.Command{Use: "stats", Short: "Cache hit rates and token totals per profile/model", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			window := strOr(since, "7d")
			cutoff, err := parseSince(window)
			if err != nil {
				return err
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			q := `SELECT master_profile, COALESCE(model_id,''), SUM(prompt_tokens), SUM(completion_tokens),
				SUM(COALESCE(cache_read_tokens,0)), SUM(COALESCE(cache_creation_tokens,0)),
				SUM(COALESCE(estimated_cost_usd,0)), COUNT(*)
				FROM runs WHERE created_at >= ?`
			qargs := []any{cutoff}
			if profile != "" {
				q += ` AND master_profile=?`
				qargs = append(qargs, profile)
			}
			if model != "" {
				q += ` AND model_id=?`
				qargs = append(qargs, model)
			}
			q += ` GROUP BY master_profile, model_id ORDER BY SUM(COALESCE(cache_read_tokens,0)) DESC LIMIT 30`
			rows, err := db.Query(q, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			// JSON contract mirrors src/tag/cmd/observability.py:_cmd_cache_stats (#541):
			// runs_total, hit_rate (null when no prompt tokens), total_cost_usd,
			// window_days (the raw --since string), cache_creation_tokens,
			// savings_usd, write_premium_usd, net_savings_usd.
			type row struct {
				Profile      string   `json:"profile"`
				Model        string   `json:"model"`
				WindowDays   string   `json:"window_days"`
				RunsTotal    int      `json:"runs_total"`
				PromptTok    int      `json:"prompt_tokens"`
				CompTok      int      `json:"completion_tokens"`
				CacheRead    int      `json:"cache_read_tokens"`
				CacheCreate  int      `json:"cache_creation_tokens"`
				HitRate      *float64 `json:"hit_rate"`
				SavingsUSD   float64  `json:"savings_usd"`
				WritePremium float64  `json:"write_premium_usd"`
				NetSavings   float64  `json:"net_savings_usd"`
				TotalCostUSD float64  `json:"total_cost_usd"`
			}
			out := []row{}
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.Profile, &r.Model, &r.PromptTok, &r.CompTok, &r.CacheRead, &r.CacheCreate, &r.TotalCostUSD, &r.RunsTotal); err != nil {
					return err
				}
				r.WindowDays = window
				if r.PromptTok > 0 {
					hr := round4(float64(r.CacheRead) / float64(r.PromptTok))
					r.HitRate = &hr
				}
				sav, wp, net := cacheSavings(r.CacheRead, r.CacheCreate, r.Model)
				r.SavingsUSD = round6(sav)
				r.WritePremium = round6(wp)
				r.NetSavings = round6(net)
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			// warnThreshold==0 disables the warn gate (mirrors Python `if warn_threshold`).
			warned := false
			if flagJSON {
				for _, r := range out {
					if warnThreshold != 0 && r.HitRate != nil && *r.HitRate < warnThreshold {
						warned = true
					}
				}
				if err := emitJSON(out); err != nil {
					return err
				}
				if warned {
					os.Exit(1)
				}
				return nil
			}
			if len(out) == 0 {
				fmt.Println("No run data found for the given filters.")
				return nil
			}
			fmt.Printf("%-16s %-24s %8s %8s %10s %8s\n", "Profile", "Model", "Prompt", "Cache", "HitRate", "Runs")
			fmt.Println(strings.Repeat("-", 80))
			for _, r := range out {
				hit := 0.0
				if r.PromptTok > 0 {
					hit = float64(r.CacheRead) / float64(r.PromptTok)
				}
				if warnThreshold != 0 && r.PromptTok > 0 && hit < warnThreshold {
					warned = true
					fmt.Printf("  [WARN] %s: hit rate %.1f%% below threshold %.0f%%\n", r.Profile, hit*100, warnThreshold*100)
				}
				fmt.Printf("%-16s %-24s %8d %8d %9.1f%% %8d\n", r.Profile, truncate(r.Model, 24), r.PromptTok, r.CacheRead, hit*100, r.RunsTotal)
			}
			if warned {
				os.Exit(1)
			}
			return nil
		}}
	stats.Flags().StringVar(&profile, "profile", "", "filter by profile")
	stats.Flags().StringVar(&model, "model", "", "filter by model id")
	stats.Flags().StringVar(&since, "since", "7d", "time window: 7d, 2w, 1m")
	stats.Flags().Float64Var(&warnThreshold, "warn-threshold", 0, "warn (exit 1) when hit rate below this fraction, e.g. 0.5")

	var trProfile, trSince string
	var trBuckets int
	trend := &cobra.Command{Use: "trend", Short: "Cache hit-rate trend over time", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			days, err := parseSinceDays(strOr(trSince, "30d"))
			if err != nil {
				if flagJSON {
					b, _ := json.Marshal(map[string]any{"error": err.Error()})
					fmt.Println(string(b))
					return nil
				}
				return err
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02T15:04:05")
			q := `SELECT date(created_at) AS day, SUM(prompt_tokens), SUM(COALESCE(cache_read_tokens,0)) FROM runs WHERE created_at >= ?`
			qargs := []any{cutoff}
			if trProfile != "" {
				q += ` AND master_profile=?`
				qargs = append(qargs, trProfile)
			}
			q += ` GROUP BY day ORDER BY day`
			rows, err := db.Query(q, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			type dv struct{ pt, crt int }
			data := map[string]dv{}
			for rows.Next() {
				var day string
				var pt, crt int
				if err := rows.Scan(&day, &pt, &crt); err != nil {
					return err
				}
				data[day] = dv{pt, crt}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			today := time.Now().UTC()
			start := today.AddDate(0, 0, -(days - 1))
			type dayrec struct {
				day     string
				pt, crt int
			}
			series := make([]dayrec, days)
			for i := 0; i < days; i++ {
				d := start.AddDate(0, 0, i).Format("2006-01-02")
				v := data[d]
				series[i] = dayrec{d, v.pt, v.crt}
			}
			nBuckets := trBuckets
			if nBuckets < 1 {
				nBuckets = 1
			}
			if nBuckets > days {
				nBuckets = days
			}
			size := (days + nBuckets - 1) / nBuckets
			type bucket struct {
				Start     string  `json:"start"`
				End       string  `json:"end"`
				PromptTok int     `json:"prompt_tokens"`
				CacheRead int     `json:"cache_read_tokens"`
				HitRate   float64 `json:"hit_rate"`
			}
			grouped := []bucket{}
			for b := 0; b < days; b += size {
				end := b + size
				if end > days {
					end = days
				}
				chunk := series[b:end]
				if len(chunk) == 0 {
					continue
				}
				pt, crt := 0, 0
				for _, c := range chunk {
					pt += c.pt
					crt += c.crt
				}
				hit := 0.0
				if pt > 0 {
					hit = float64(crt) / float64(pt)
				}
				grouped = append(grouped, bucket{chunk[0].day, chunk[len(chunk)-1].day, pt, crt,
					float64(int(hit*10000+0.5)) / 10000})
			}
			if flagJSON {
				b, _ := json.MarshalIndent(map[string]any{"profile": nullStr(trProfile), "since": strOr(trSince, "30d"), "buckets": grouped}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			label := "all profiles"
			if trProfile != "" {
				label = trProfile
			}
			fmt.Printf("Cache hit rate — %s — last %s\n\n", label, strOr(trSince, "30d"))
			barWidth := 40
			for _, g := range grouped {
				span := g.Start
				if g.Start != g.End {
					span = g.Start + ".." + g.End
				}
				bar := strings.Repeat("█", int(g.HitRate*float64(barWidth)))
				fmt.Printf("  %-24s  %-*s  %.0f%%\n", span, barWidth, bar, g.HitRate*100)
			}
			return nil
		}}
	trend.Flags().StringVar(&trProfile, "profile", "", "filter by profile")
	trend.Flags().StringVar(&trSince, "since", "30d", "time window: 30d, 4w, 1m")
	trend.Flags().IntVar(&trBuckets, "buckets", 14, "number of buckets")

	var tipsProfile string
	tips := &cobra.Command{Use: "tips", Short: "Actionable prompt-cache recommendations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if tipsProfile == "" {
				return fmt.Errorf("--profile is required for cache tips")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT prompt, COALESCE(cache_read_tokens,0), prompt_tokens, created_at FROM runs WHERE master_profile=? ORDER BY created_at DESC LIMIT 20`, tipsProfile)
			if err != nil {
				return err
			}
			defer rows.Close()
			type rr struct {
				prompt  string
				crt, pt int
			}
			var recs []rr
			for rows.Next() {
				var r rr
				if err := rows.Scan(&r.prompt, &r.crt, &r.pt, new(string)); err != nil {
					return err
				}
				recs = append(recs, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			fmt.Printf("Cache tips for profile: %s\n\n", tipsProfile)
			if len(recs) == 0 {
				fmt.Println("  No run history found for this profile.")
				return nil
			}
			shas := make([]string, len(recs))
			for i, r := range recs {
				h := sha256.Sum256([]byte(r.prompt))
				shas[i] = hex.EncodeToString(h[:])
			}
			stablePairs := 0
			for i := 0; i+1 < len(shas); i++ {
				if shas[i] == shas[i+1] {
					stablePairs++
				}
			}
			denom := len(shas) - 1
			if denom < 1 {
				denom = 1
			}
			stability := float64(stablePairs) / float64(denom)
			totalPt, totalCrt := 0, 0
			for _, r := range recs {
				totalPt += r.pt
				totalCrt += r.crt
			}
			hitRate := 0.0
			if totalPt > 0 {
				hitRate = float64(totalCrt) / float64(totalPt)
			}
			estTokens := float64(len(strings.Fields(recs[0].prompt))) * 1.3
			if hitRate < 0.3 {
				fmt.Printf("  [WARN] Cache hit rate is %.0f%% over the last %d runs (threshold: 30%%)\n", hitRate*100, len(recs))
			} else {
				fmt.Printf("  [OK]   Cache hit rate is %.0f%% over the last %d runs\n", hitRate*100, len(recs))
			}
			if estTokens > 1024 {
				fmt.Printf("  [INFO] System prompt is ~%s tokens — large enough to benefit from caching\n", budgetComma(int64(estTokens)))
			} else {
				fmt.Printf("  [INFO] System prompt is ~%s tokens — below 1024 token caching threshold\n", budgetComma(int64(estTokens)))
			}
			fmt.Println("\nRecommendations:")
			n := 0
			if stability < 0.5 {
				n++
				fmt.Printf("  %d. System prompt SHA changed in %d/%d consecutive runs.\n", n, denom-stablePairs, denom)
				fmt.Println("     A volatile prompt prevents cache reuse. Move dynamic content to the user-turn message.")
			}
			if hitRate < 0.3 && estTokens > 1024 {
				n++
				fmt.Printf("  %d. Add a cache_control breakpoint at the end of your static system prompt block:\n", n)
				fmt.Println("     {\"cache_control\": {\"type\": \"ephemeral\"}} in your system message.")
			}
			if n == 0 {
				fmt.Println("  No specific issues detected — cache appears healthy.")
			}
			return nil
		}}
	tips.Flags().StringVar(&tipsProfile, "profile", "", "profile (required)")

	c.AddCommand(stats, trend, tips)
	root.AddCommand(c)
}

// nullStr returns nil for an empty string (so JSON emits null, matching
// Python's None for an unset --profile filter), else the string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// parseSince converts "7d"/"2w"/"1m" to an ISO cutoff timestamp.
func parseSince(since string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(since))
	if len(s) < 2 {
		return "", fmt.Errorf("invalid --since value; expected e.g. 7d, 2w, 1m")
	}
	// The numeric part must be all digits (a leading '-' would parse as a
	// negative and silently produce a FUTURE cutoff); reject it like Python.
	numPart := s[:len(s)-1]
	n, err := strconv.Atoi(numPart)
	if err != nil || n < 0 || strings.HasPrefix(numPart, "-") || strings.HasPrefix(numPart, "+") {
		return "", fmt.Errorf("invalid --since value; expected e.g. 7d, 2w, 1m")
	}
	var d time.Duration
	switch s[len(s)-1] {
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	case 'w':
		d = time.Duration(n) * 7 * 24 * time.Hour
	case 'm':
		d = time.Duration(n) * 30 * 24 * time.Hour
	default:
		return "", fmt.Errorf("invalid --since unit; expected d, w, or m")
	}
	return time.Now().UTC().Add(-d).Format("2006-01-02T15:04:05"), nil
}
