package cli

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/store"
)

// aopProfileStat is the per-profile rollup shown by `tag agentops`.
type aopProfileStat struct {
	Profile          string         `json:"profile"`
	Runs             int            `json:"runs"`
	PromptTokens     int64          `json:"prompt_tokens"`
	CompletionTokens int64          `json:"completion_tokens"`
	TotalTokens      int64          `json:"total_tokens"`
	EstimatedCostUSD float64        `json:"estimated_cost_usd"`
	Statuses         map[string]int `json:"statuses"`
}

// aopSummary is the full session-observability summary.
type aopSummary struct {
	TotalRuns        int              `json:"total_runs"`
	PromptTokens     int64            `json:"prompt_tokens"`
	CompletionTokens int64            `json:"completion_tokens"`
	TotalTokens      int64            `json:"total_tokens"`
	EstimatedCostUSD float64          `json:"estimated_cost_usd"`
	Statuses         map[string]int   `json:"statuses"`
	Profiles         []aopProfileStat `json:"profiles"`
}

// aopSummarize reads the existing `runs` table and rolls up per-profile run
// counts, token totals, and statuses.
func aopSummarize(db *store.DB) (aopSummary, error) {
	sum := aopSummary{Statuses: map[string]int{}}
	rows, err := db.Query(`SELECT master_profile, status,
		COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0),
		COALESCE(estimated_cost_usd,0) FROM runs`)
	if err != nil {
		return sum, err
	}
	defer rows.Close()

	byProfile := map[string]*aopProfileStat{}
	for rows.Next() {
		var profile, status string
		var pt, ct int64
		var cost float64
		if err := rows.Scan(&profile, &status, &pt, &ct, &cost); err != nil {
			return sum, err
		}
		sum.TotalRuns++
		sum.PromptTokens += pt
		sum.CompletionTokens += ct
		sum.EstimatedCostUSD += cost
		sum.Statuses[status]++

		ps := byProfile[profile]
		if ps == nil {
			ps = &aopProfileStat{Profile: profile, Statuses: map[string]int{}}
			byProfile[profile] = ps
		}
		ps.Runs++
		ps.PromptTokens += pt
		ps.CompletionTokens += ct
		ps.TotalTokens += pt + ct
		ps.EstimatedCostUSD += cost
		ps.Statuses[status]++
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}
	sum.TotalTokens = sum.PromptTokens + sum.CompletionTokens

	names := make([]string, 0, len(byProfile))
	for n := range byProfile {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sum.Profiles = append(sum.Profiles, *byProfile[n])
	}
	return sum, nil
}

func registerAgentops(root *cobra.Command, app *App) {
	aop := &cobra.Command{
		Use:     "agentops",
		Short:   "Session observability: per-profile runs, tokens, statuses",
		GroupID: "obs",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			summary, err := aopSummarize(db)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if flagJSON {
				return emitJSON(summary)
			}
			if summary.TotalRuns == 0 {
				fmt.Println("No runs recorded.")
				return nil
			}
			fmt.Printf("AgentOps — %d runs, %d tokens (prompt=%d completion=%d), est. cost $%.6f\n",
				summary.TotalRuns, summary.TotalTokens,
				summary.PromptTokens, summary.CompletionTokens, summary.EstimatedCostUSD)
			fmt.Printf("Statuses: %s\n", aopStatusLine(summary.Statuses))
			fmt.Println()
			fmt.Printf("  %-20s %-6s %-12s %-10s %s\n", "PROFILE", "RUNS", "TOKENS", "COST$", "STATUSES")
			fmt.Println("  " + repeatDash(72))
			for _, p := range summary.Profiles {
				fmt.Printf("  %-20s %-6d %-12d %-10.6f %s\n",
					p.Profile, p.Runs, p.TotalTokens, p.EstimatedCostUSD, aopStatusLine(p.Statuses))
			}
			return nil
		},
	}
	root.AddCommand(aop)
}

// aopStatusLine renders a status map deterministically ("done=3 failed=1").
func aopStatusLine(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=%d", k, m[k])
	}
	if out == "" {
		return "(none)"
	}
	return out
}

// repeatDash returns n dash characters.
func repeatDash(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}
