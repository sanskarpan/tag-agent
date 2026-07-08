package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/store"
)

// PRD-040: tag notify add/list/test/remove/enable/disable — notification hooks.
// Port of src/tag/cmd/agent_tools.py:cmd_notify + src/tag/notifications.py.
var (
	notifChannels = map[string]bool{"slack": true, "email": true, "desktop": true, "webhook": true}
	notifEvents   = map[string]bool{
		"run.completed": true, "run.failed": true, "run.started": true,
		"budget.warning": true, "budget.exceeded": true,
		"queue.done": true, "queue.failed": true,
		"loop.completed": true, "loop.failed": true,
	}
	notifTemplateVars = []string{"run_id", "profile", "duration", "tokens_used", "cost_usd", "status", "error_message", "task", "event"}
)

func registerNotify(root *cobra.Command, app *App) {
	n := &cobra.Command{Use: "notify", Short: "Notification hooks for run/budget/queue events", GroupID: "obs"}

	var event, channel, profile, configJSON, template string
	add := &cobra.Command{Use: "add", Short: "Add a notification hook", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if event == "" {
				event = "run.completed"
			}
			if channel == "" {
				channel = "desktop"
			}
			if !notifEvents[event] {
				return fmt.Errorf("event must be one of %s, got %q", strings.Join(sortedSet(notifEvents), ", "), event)
			}
			if !notifChannels[channel] {
				return fmt.Errorf("channel must be one of %s, got %q", strings.Join(sortedSet(notifChannels), ", "), channel)
			}
			var cfgData map[string]any
			if err := json.Unmarshal([]byte(strOr(configJSON, "{}")), &cfgData); err != nil {
				return fmt.Errorf("invalid config JSON: %w", err)
			}
			cfgBytes, _ := json.Marshal(cfgData)
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id := uuid.NewString()[:12]
			var prof any
			if profile != "" {
				prof = profile
			}
			_, err = db.Exec(`INSERT INTO notification_hooks(id,profile,event,channel,config_json,template,enabled,created_at) VALUES(?,?,?,?,?,?,1,?)`,
				id, prof, event, channel, string(cfgBytes), template, time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			outJSON(map[string]any{"id": id, "channel": channel, "event": event},
				fmt.Sprintf("Notification hook added: %s  (%s on %s)", id, channel, event))
			return nil
		}}
	add.Flags().StringVar(&event, "event", "", "event (default run.completed)")
	add.Flags().StringVar(&channel, "channel", "", "channel: slack|email|desktop|webhook (default desktop)")
	add.Flags().StringVar(&profile, "profile", "", "restrict to profile (default all)")
	add.Flags().StringVar(&configJSON, "config-json", "{}", "channel config as JSON")
	add.Flags().StringVar(&template, "template", "", "message template with {{vars}}")

	var listProfile string
	list := &cobra.Command{Use: "list", Short: "List notification hooks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			hooks, err := listHooks(db, listProfile)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(hooks)
			}
			if len(hooks) == 0 {
				fmt.Println("No notification hooks configured.")
				return nil
			}
			for _, h := range hooks {
				status := "✗"
				if h["enabled"].(bool) {
					status = "✓"
				}
				prof := str(h["profile"])
				if prof == "" {
					prof = "*"
				}
				fmt.Printf("%s %s  %-10s %-20s profile=%s\n", status, str(h["id"])[:8], h["channel"], h["event"], prof)
			}
			return nil
		}}
	list.Flags().StringVar(&listProfile, "profile", "", "filter by profile")

	test := &cobra.Command{Use: "test <hook-id>", Short: "Render a hook's template with sample context (offline; no network delivery)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			hooks, err := listHooks(db, "")
			if err != nil {
				return err
			}
			var hook map[string]any
			for _, h := range hooks {
				if strings.HasPrefix(str(h["id"]), args[0]) {
					hook = h
					break
				}
			}
			if hook == nil {
				return fmt.Errorf("hook not found: %q", args[0])
			}
			ctx := map[string]string{
				"run_id": "test-run-001", "profile": "test", "duration": "0s",
				"tokens_used": "0", "cost_usd": "0.00", "status": "completed",
				"error_message": "", "task": "Test notification", "event": "test",
			}
			rendered := renderTemplate(strOr(str(hook["template"]), "TAG {{event}}: run {{run_id}} {{status}}"), ctx)
			fmt.Printf("✓ Hook %s (%s on %s) — rendered message:\n%s\n", str(hook["id"])[:8], hook["channel"], hook["event"], rendered)
			return nil
		}}

	remove := &cobra.Command{Use: "remove <hook-id>", Short: "Remove a notification hook", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			id, err := resolveHookID(db, args[0])
			if err != nil {
				return err
			}
			db.Exec(`DELETE FROM notification_log WHERE hook_id=?`, id)
			if _, err := db.Exec(`DELETE FROM notification_hooks WHERE id=?`, id); err != nil {
				return err
			}
			fmt.Printf("Notification hook removed: %s\n", id[:8])
			return nil
		}}

	setEnabled := func(use, short string, enabled bool) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				db, err := app.OpenDB()
				if err != nil {
					return err
				}
				id, err := resolveHookID(db, args[0])
				if err != nil {
					return err
				}
				v := 0
				if enabled {
					v = 1
				}
				if _, err := db.Exec(`UPDATE notification_hooks SET enabled=? WHERE id=?`, v, id); err != nil {
					return err
				}
				state := "disabled"
				if enabled {
					state = "enabled"
				}
				fmt.Printf("Notification hook %s: %s\n", state, id[:8])
				return nil
			}}
	}

	n.AddCommand(add, list, test, remove,
		setEnabled("enable <hook-id>", "Enable a hook", true),
		setEnabled("disable <hook-id>", "Disable a hook", false))
	root.AddCommand(n)
}

func listHooks(db *store.DB, profile string) ([]map[string]any, error) {
	var rows *sql.Rows
	var err error
	if profile != "" {
		rows, err = db.Query(`SELECT id,profile,event,channel,config_json,template,enabled FROM notification_hooks
			WHERE (profile=? OR profile IS NULL) AND enabled=1 ORDER BY created_at`, profile)
	} else {
		rows, err = db.Query(`SELECT id,profile,event,channel,config_json,template,enabled FROM notification_hooks ORDER BY created_at`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, event, channel, cfg, template string
		var prof sql.NullString
		var enabled int
		if err := rows.Scan(&id, &prof, &event, &channel, &cfg, &template, &enabled); err != nil {
			return nil, err
		}
		var cfgData map[string]any
		_ = json.Unmarshal([]byte(strOr(cfg, "{}")), &cfgData)
		out = append(out, map[string]any{
			"id": id, "profile": prof.String, "event": event, "channel": channel,
			"config": cfgData, "template": template, "enabled": enabled != 0,
		})
	}
	return out, rows.Err()
}

// resolveHookID resolves a full hook id from an unambiguous prefix (matches the
// truncated 8-char id shown by `notify list`). Errors if no or multiple matches.
func resolveHookID(db *store.DB, prefix string) (string, error) {
	rows, err := db.Query(`SELECT id FROM notification_hooks WHERE id LIKE ? || '%'`, prefix)
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
		return "", fmt.Errorf("hook not found: %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous hook id %q matches %d hooks", prefix, len(matches))
	}
}

// sortedSet returns the keys of a set in sorted order.
func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renderTemplate does simple {{var}} substitution from the notify allow-list.
func renderTemplate(tmpl string, ctx map[string]string) string {
	result := tmpl
	for _, k := range notifTemplateVars {
		result = strings.ReplaceAll(result, "{{"+k+"}}", ctx[k])
	}
	return result
}
