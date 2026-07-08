package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func registerBudget(root *cobra.Command, app *App) {
	var profile string
	var maxTokens int64
	var period string
	var warnPct float64
	b := &cobra.Command{Use: "budget", Short: "Per-profile token budget enforcement", GroupID: "tools"}
	b.PersistentFlags().StringVar(&profile, "profile", "", "profile")

	set := &cobra.Command{Use: "set", Short: "Set a budget", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prof := budgetProfile(app, profile, args)
			if maxTokens <= 0 {
				return fmt.Errorf("max-tokens must be > 0")
			}
			if maxTokens > (1<<63 - 1) {
				return fmt.Errorf("max-tokens too large")
			}
			if warnPct <= 0 || warnPct >= 1 {
				return fmt.Errorf("warn-pct must be in (0, 1)")
			}
			switch period {
			case "daily", "weekly", "monthly":
			default:
				return fmt.Errorf("--period must be one of daily/weekly/monthly, got %q", period)
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			now := time.Now().UTC().Format(time.RFC3339)
			_, err = db.Exec(`INSERT INTO token_budgets(id,profile,period,max_tokens,warn_pct,enabled,created_at,updated_at)
				VALUES(?,?,?,?,?,1,?,?) ON CONFLICT(profile) DO UPDATE SET period=excluded.period,max_tokens=excluded.max_tokens,warn_pct=excluded.warn_pct,enabled=1,updated_at=excluded.updated_at`,
				uuid.NewString()[:12], prof, period, maxTokens, warnPct, now, now)
			if err != nil {
				return err
			}
			fmt.Printf("Budget set for '%s': %d tokens/%s (warn at %d%%)\n", prof, maxTokens, period, int(warnPct*100))
			return nil
		}}
	set.Flags().Int64Var(&maxTokens, "max-tokens", 0, "max tokens")
	set.Flags().StringVar(&period, "period", "daily", "period")
	set.Flags().Float64Var(&warnPct, "warn-pct", 0.8, "warn fraction (0,1)")

	get := &cobra.Command{Use: "get", Short: "Show a profile budget", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prof := budgetProfile(app, profile, args)
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var id, pd string
			var mt int64
			var wp float64
			var en int
			err = db.QueryRow(`SELECT id,max_tokens,period,warn_pct,enabled FROM token_budgets WHERE profile=?`, prof).Scan(&id, &mt, &pd, &wp, &en)
			if errors.Is(err, sql.ErrNoRows) {
				outJSON(map[string]any{"profile": prof, "budget": nil}, fmt.Sprintf("No budget set for profile '%s'.", prof))
				return nil
			}
			if err != nil {
				return err
			}
			outJSON(map[string]any{"id": id, "profile": prof, "period": pd, "max_tokens": mt, "warn_pct": wp, "enabled": en != 0},
				fmt.Sprintf("%s: %d tokens/%s (warn %d%%)", prof, mt, pd, int(wp*100)))
			return nil
		}}
	list := &cobra.Command{Use: "list", Short: "List budgets",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id,profile,period,max_tokens,warn_pct,enabled FROM token_budgets ORDER BY profile`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type budgetRow struct {
				ID        string  `json:"id"`
				Profile   string  `json:"profile"`
				Period    string  `json:"period"`
				MaxTokens int64   `json:"max_tokens"`
				WarnPct   float64 `json:"warn_pct"`
				Enabled   bool    `json:"enabled"`
			}
			out := []budgetRow{}
			for rows.Next() {
				var r budgetRow
				var en int
				if err := rows.Scan(&r.ID, &r.Profile, &r.Period, &r.MaxTokens, &r.WarnPct, &en); err != nil {
					return err
				}
				r.Enabled = en != 0
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No token budgets configured.")
				return nil
			}
			for _, r := range out {
				st := "✓"
				if !r.Enabled {
					st = "✗"
				}
				fmt.Printf("%s %-30s %10d tokens/%s\n", st, r.Profile, r.MaxTokens, r.Period)
			}
			return nil
		}}
	remove := &cobra.Command{Use: "remove", Short: "Remove a budget", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prof := budgetProfile(app, profile, args)
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r, err := db.Exec(`DELETE FROM token_budgets WHERE profile=?`, prof)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				return fmt.Errorf("no budget for '%s'", prof)
			}
			fmt.Printf("Budget removed for '%s'.\n", prof)
			return nil
		}}
	check := &cobra.Command{Use: "check", Short: "Check a profile's budget usage vs limit", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			prof := budgetProfile(app, profile, args)
			var id, pd string
			var mt int64
			var wp float64
			var en int
			err = db.QueryRow(`SELECT id,period,max_tokens,warn_pct,enabled FROM token_budgets WHERE profile=?`, prof).Scan(&id, &pd, &mt, &wp, &en)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if errors.Is(err, sql.ErrNoRows) || en == 0 {
				if flagJSON {
					b, _ := json.Marshal(map[string]any{"profile": prof, "budget": nil, "unlimited": true})
					fmt.Println(string(b))
				} else {
					fmt.Printf("No budget configured for '%s' — unlimited.\n", prof)
				}
				return nil
			}
			days := map[string]int{"daily": 1, "weekly": 7, "monthly": 30}[pd]
			if days == 0 {
				days = 1
			}
			windowStart := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
			var used int64
			if err := db.QueryRow(`SELECT COALESCE(SUM(prompt_tokens + completion_tokens),0) FROM runs WHERE master_profile=? AND created_at >= ?`, prof, windowStart).Scan(&used); err != nil {
				return err
			}
			pct := 0.0
			if mt > 0 {
				pct = float64(used) / float64(mt)
			}
			pctRounded := math.Round(pct*100*10) / 10
			warn := pct >= wp
			exceeded := pct >= 1.0
			if exceeded {
				return fmt.Errorf("Token budget exceeded for profile '%s': %d / %d tokens used (%s)", prof, used, mt, pd)
			}
			budget := map[string]any{"id": id, "profile": prof, "period": pd, "max_tokens": mt, "warn_pct": wp, "enabled": true}
			if flagJSON {
				// Emit pct as a float literal (e.g. 0.0) to match Python's
				// json.dumps(round(pct*100, 1)) rather than Go's bare 0.
				b, _ := json.MarshalIndent(map[string]any{"allowed": true, "budget": budget, "profile": prof,
					"used": used, "limit": mt, "period": pd, "pct": json.RawMessage(budgetTrim(pctRounded)), "warn": warn}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			icon := "✓"
			if warn {
				icon = "⚠"
			}
			fmt.Printf("%s %s: %s/%s tokens (%s%%) [%s]\n", icon, prof,
				budgetComma(used), budgetComma(mt), budgetTrim(pctRounded), pd)
			return nil
		}}
	b.AddCommand(set, get, list, remove, check)
	root.AddCommand(b)
}

// budgetProfile resolves the effective profile for a budget subcommand.
// Python binds the profile via --profile only and argparse rejects a stray
// positional; the subcommands use cobra.NoArgs so a stray positional is a usage
// error (exit 2) rather than being silently swallowed onto the default profile
// (#533). The profile comes solely from --profile (or the default).
func budgetProfile(app *App, flag string, _ []string) string {
	return app.profile(flag)
}

// budgetComma formats an integer with thousands separators (mirrors Python's
// {:,} used in the budget output).
func budgetComma(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// budgetTrim renders a rounded percentage the way Python prints a float (no
// trailing .0 for whole numbers is NOT done by Python round(); Python prints
// e.g. 12.5 and 0.0). Match Python str(float): keep one decimal.
func budgetTrim(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	return s
}
