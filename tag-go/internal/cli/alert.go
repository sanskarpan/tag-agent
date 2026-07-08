package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/store"
)

// registerAlert wires alert rules + firings: alert create/list/check/firings/delete.
// Port of src/tag/cmd/prd_clusters.py:cmd_alert + alerts.py.
var (
	alertMetrics = map[string]bool{
		"eval_pass_rate": true, "eval_score": true, "span_error_rate": true,
		"p95_latency_ms": true, "cost_usd_per_run": true, "cache_hit_rate": true,
		"memory_count": true,
	}
	alertSeverities  = map[string]bool{"info": true, "warning": true, "critical": true}
	alertConditions  = map[string]bool{"lt": true, "gt": true, "lte": true, "gte": true}
	alertCondLabels  = map[string]string{"lt": "<", "gt": ">", "lte": "<=", "gte": ">="}
	alertCooldownSec = 3600.0
)

func registerAlert(root *cobra.Command, app *App) {
	a := &cobra.Command{Use: "alert", Short: "Alert rules and firing management", GroupID: "obs"}

	var severity, profile string
	create := &cobra.Command{Use: "create <name> <metric> <condition> <threshold>", Short: "Create an alert rule",
		Args: cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, metric, condition := strings.TrimSpace(args[0]), args[1], args[2]
			if name == "" {
				return fmt.Errorf("alert rule name must not be empty")
			}
			if !alertMetrics[metric] {
				return fmt.Errorf("unknown metric: %q", metric)
			}
			if !alertConditions[condition] {
				return fmt.Errorf("unknown condition: %q; must be lt/gt/lte/gte", condition)
			}
			if !alertSeverities[severity] {
				return fmt.Errorf("unknown severity: %q", severity)
			}
			var threshold float64
			if _, err := fmt.Sscan(args[3], &threshold); err != nil {
				return fmt.Errorf("invalid threshold %q", args[3])
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id := uuid.NewString()
			var prof any
			if profile != "" {
				prof = profile
			}
			_, err = db.Exec(`INSERT INTO alert_rules(id,name,metric,condition,threshold,severity,profile,suite,enabled,notify_channels,created_at,last_triggered_at)
				VALUES(?,?,?,?,?,?,?,NULL,1,'',?,NULL)`,
				id, name, metric, condition, threshold, severity, prof, time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			fmt.Printf("Created rule '%s' (id=%s)\n", name, id)
			return nil
		}}
	create.Flags().StringVar(&severity, "severity", "warning", "info|warning|critical")
	create.Flags().StringVar(&profile, "profile", "", "restrict to profile")

	list := &cobra.Command{Use: "list", Short: "List enabled alert rules", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rules, err := listAlertRules(db, true)
			if err != nil {
				return err
			}
			if flagJSON {
				if rules == nil {
					rules = []alertRule{}
				}
				return emitJSON(rules)
			}
			for _, r := range rules {
				fmt.Printf("%s  %-30s %s %s %g [%s]\n", r.ID[:8], r.Name, r.Metric, r.Condition, r.Threshold, r.Severity)
			}
			return nil
		}}

	check := &cobra.Command{Use: "check", Short: "Evaluate rules against the current metric snapshot", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			snapshot, err := computeMetricSnapshot(db, app.profile(profile))
			if err != nil {
				return err
			}
			firings, err := checkAlerts(db, snapshot, alertCooldownSec)
			if err != nil {
				return err
			}
			if flagJSON {
				if firings == nil {
					firings = []alertFiring{}
				}
				return emitJSON(firings)
			}
			if len(firings) == 0 {
				fmt.Println("No alerts firing")
				return nil
			}
			for _, f := range firings {
				fmt.Println(f.Message)
			}
			return nil
		}}
	check.Flags().StringVar(&profile, "profile", "", "profile for the metric snapshot")

	var limit int
	firings := &cobra.Command{Use: "firings", Short: "Show recent alert firings", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id, rule_id, rule_name, metric, actual_value, threshold, severity, fired_at, COALESCE(resolved_at,''), message FROM alert_firings ORDER BY fired_at DESC LIMIT ?`, limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			type firing struct {
				ID          string  `json:"id"`
				RuleID      string  `json:"rule_id"`
				RuleName    string  `json:"rule_name"`
				Metric      string  `json:"metric"`
				ActualValue float64 `json:"actual_value"`
				Threshold   float64 `json:"threshold"`
				Severity    string  `json:"severity"`
				FiredAt     string  `json:"fired_at"`
				ResolvedAt  string  `json:"resolved_at"`
				Message     string  `json:"message"`
			}
			out := []firing{}
			for rows.Next() {
				var f firing
				rows.Scan(&f.ID, &f.RuleID, &f.RuleName, &f.Metric, &f.ActualValue, &f.Threshold, &f.Severity, &f.FiredAt, &f.ResolvedAt, &f.Message)
				out = append(out, f)
			}
			if flagJSON {
				return emitJSON(out)
			}
			for _, f := range out {
				fmt.Printf("[%s] %s: %.4f at %s\n", f.Severity, f.RuleName, f.ActualValue, f.FiredAt)
			}
			return nil
		}}
	firings.Flags().IntVar(&limit, "limit", 20, "max firings")

	del := &cobra.Command{Use: "delete <rule-id>", Short: "Delete an alert rule", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id, err := resolveAlertRuleID(db, args[0])
			if err != nil {
				fmt.Println("Not found")
				return err
			}
			// FK is enforced (unlike Python's default-off sqlite3), so remove the
			// rule's firing history first, then the rule itself.
			if _, err := db.Exec(`DELETE FROM alert_firings WHERE rule_id=?`, id); err != nil {
				return err
			}
			if _, err := db.Exec(`DELETE FROM alert_rules WHERE id=?`, id); err != nil {
				return err
			}
			fmt.Println("Deleted")
			return nil
		}}

	a.AddCommand(create, list, check, firings, del)
	root.AddCommand(a)
}

type alertRule struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Metric      string  `json:"metric"`
	Condition   string  `json:"condition"`
	Threshold   float64 `json:"threshold"`
	Severity    string  `json:"severity"`
	LastTrigger string  `json:"last_triggered_at"`
}

type alertFiring struct {
	ID          string  `json:"id"`
	RuleID      string  `json:"rule_id"`
	RuleName    string  `json:"rule_name"`
	Metric      string  `json:"metric"`
	ActualValue float64 `json:"actual_value"`
	Threshold   float64 `json:"threshold"`
	Severity    string  `json:"severity"`
	FiredAt     string  `json:"fired_at"`
	ResolvedAt  *string `json:"resolved_at"`
	Message     string  `json:"message"`
}

// resolveAlertRuleID resolves a full rule id from an unambiguous prefix (matches
// the truncated 8-char id shown by `alert list`).
func resolveAlertRuleID(db *store.DB, prefix string) (string, error) {
	rows, err := db.Query(`SELECT id FROM alert_rules WHERE id LIKE ? || '%'`, prefix)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("rule not found: %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous rule id %q matches %d rules", prefix, len(matches))
	}
}

func listAlertRules(db *store.DB, enabledOnly bool) ([]alertRule, error) {
	q := `SELECT id,name,metric,condition,threshold,severity,COALESCE(last_triggered_at,'') FROM alert_rules`
	if enabledOnly {
		q += ` WHERE enabled=1`
	}
	q += ` ORDER BY created_at`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alertRule
	for rows.Next() {
		var r alertRule
		if err := rows.Scan(&r.ID, &r.Name, &r.Metric, &r.Condition, &r.Threshold, &r.Severity, &r.LastTrigger); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func evaluateAlert(condition string, actual, threshold float64) bool {
	switch condition {
	case "lt":
		return actual < threshold
	case "gt":
		return actual > threshold
	case "lte":
		return actual <= threshold
	case "gte":
		return actual >= threshold
	}
	return false
}

func buildAlertMessage(r alertRule, actual float64) string {
	op := alertCondLabels[r.Condition]
	if op == "" {
		op = r.Condition
	}
	return fmt.Sprintf("[%s] %s: %s = %.4g %s %.4g", strings.ToUpper(r.Severity), r.Name, r.Metric, actual, op, r.Threshold)
}

// checkAlerts evaluates enabled rules against metrics, persisting firings and
// honoring the cooldown suppression window (C: no duplicate firings).
func checkAlerts(db *store.DB, metrics map[string]float64, cooldownSec float64) ([]alertFiring, error) {
	rules, err := listAlertRules(db, true)
	if err != nil {
		return nil, err
	}
	nowDt := time.Now().UTC()
	now := nowDt.Format(time.RFC3339)
	var fired []alertFiring
	for _, r := range rules {
		actual, ok := metrics[r.Metric]
		if !ok || !evaluateAlert(r.Condition, actual, r.Threshold) {
			continue
		}
		if cooldownSec > 0 && r.LastTrigger != "" {
			if last, err := time.Parse(time.RFC3339, r.LastTrigger); err == nil {
				if nowDt.Sub(last).Seconds() < cooldownSec {
					continue
				}
			}
		}
		msg := buildAlertMessage(r, actual)
		firingID := uuid.NewString()
		if _, err := db.Exec(`INSERT INTO alert_firings(id,rule_id,rule_name,metric,actual_value,threshold,severity,fired_at,resolved_at,message)
			VALUES(?,?,?,?,?,?,?,?,NULL,?)`, firingID, r.ID, r.Name, r.Metric, actual, r.Threshold, r.Severity, now, msg); err != nil {
			return nil, err
		}
		if _, err := db.Exec(`UPDATE alert_rules SET last_triggered_at=? WHERE id=?`, now, r.ID); err != nil {
			return nil, err
		}
		fired = append(fired, alertFiring{
			ID: firingID, RuleID: r.ID, RuleName: r.Name, Metric: r.Metric,
			ActualValue: actual, Threshold: r.Threshold, Severity: r.Severity,
			FiredAt: now, ResolvedAt: nil, Message: msg,
		})
	}
	return fired, nil
}

// computeMetricSnapshot returns current metric values from the live tables. A
// metric whose backing table is empty stays 0.0 (matching Python). Computed:
// memory_count (semantic_memories), eval_pass_rate/eval_score (eval_runs+cases),
// span_error_rate/p95_latency_ms/cost_usd_per_run (spans). cache_hit_rate is
// derived from runs' cache vs prompt tokens.
func computeMetricSnapshot(db *store.DB, profile string) (map[string]float64, error) {
	snap := map[string]float64{}
	for m := range alertMetrics {
		snap[m] = 0.0
	}
	var memCount float64
	if err := db.QueryRow(`SELECT COUNT(*) FROM semantic_memories WHERE profile=?`, profile).Scan(&memCount); err == nil {
		snap["memory_count"] = memCount
	}

	// eval metrics: over completed runs (optionally profile-scoped), aggregate cases.
	evalWhere, evalArgs := "r.status='completed'", []any{}
	if profile != "" {
		evalWhere += " AND r.profile=?"
		evalArgs = append(evalArgs, profile)
	}
	var passSum, caseCount, scoreAvg sql.NullFloat64
	q := `SELECT SUM(c.passed), COUNT(*), AVG(c.score) FROM eval_cases c
		JOIN eval_runs r ON c.eval_run_id=r.id WHERE ` + evalWhere
	if err := db.QueryRow(q, evalArgs...).Scan(&passSum, &caseCount, &scoreAvg); err == nil && caseCount.Float64 > 0 {
		snap["eval_pass_rate"] = passSum.Float64 / caseCount.Float64
		snap["eval_score"] = scoreAvg.Float64
	}

	// span metrics: error rate, p95 latency, cost per trace.
	spanWhere, spanArgs := "", []any{}
	if profile != "" {
		spanWhere = " WHERE profile=?"
		spanArgs = append(spanArgs, profile)
	}
	var errRate sql.NullFloat64
	if err := db.QueryRow(`SELECT SUM(CASE WHEN status='error' THEN 1 ELSE 0 END)*1.0/MAX(COUNT(*),1) FROM spans`+spanWhere, spanArgs...).Scan(&errRate); err == nil && errRate.Valid {
		snap["span_error_rate"] = errRate.Float64
	}
	// p95 latency
	drows, err := db.Query(`SELECT duration_ms FROM spans`+spanWhere+` ORDER BY duration_ms`, spanArgs...)
	if err == nil {
		var durs []float64
		for drows.Next() {
			var d sql.NullInt64
			drows.Scan(&d)
			if d.Valid {
				durs = append(durs, float64(d.Int64))
			}
		}
		drows.Close()
		if n := len(durs); n > 0 {
			idx := int(float64(n)*0.95) - 1
			if idx < 0 {
				idx = 0
			}
			snap["p95_latency_ms"] = durs[idx]
		}
	}
	// cost per run: average per-trace cost.
	var costPerRun sql.NullFloat64
	costWhere := spanWhere
	if costWhere == "" {
		costWhere = " WHERE cost_usd IS NOT NULL"
	} else {
		costWhere += " AND cost_usd IS NOT NULL"
	}
	if err := db.QueryRow(`SELECT AVG(t) FROM (SELECT SUM(cost_usd) t FROM spans`+costWhere+` GROUP BY trace_id)`, spanArgs...).Scan(&costPerRun); err == nil && costPerRun.Valid {
		snap["cost_usd_per_run"] = costPerRun.Float64
	}

	// cache hit rate from runs' cache vs prompt tokens.
	runWhere, runArgs := "", []any{}
	if profile != "" {
		runWhere = " WHERE master_profile=?"
		runArgs = append(runArgs, profile)
	}
	var cacheRead, promptTok sql.NullFloat64
	if err := db.QueryRow(`SELECT SUM(cache_read_tokens), SUM(prompt_tokens) FROM runs`+runWhere, runArgs...).Scan(&cacheRead, &promptTok); err == nil && promptTok.Float64 > 0 {
		snap["cache_hit_rate"] = cacheRead.Float64 / promptTok.Float64
	}
	return snap, nil
}
