package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// registerHooks wires TAG lifecycle event hooks: hooks list/log/test.
// Port of src/tag/cmd/workflow_mgmt.py:cmd_hooks + _fire_hooks/_execute_hook.
// Shell hooks run with shell-safe {{var}} interpolation and a 30s timeout.
// The webhook hook type is not fired here (needs the shared SSRF guard).
func registerHooks(root *cobra.Command, app *App) {
	h := &cobra.Command{Use: "hooks", Short: "Manage and test TAG lifecycle event hooks", GroupID: "tools"}

	list := &cobra.Command{Use: "list", Short: "List configured hooks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			hooksCfg := app.Cfg.Section("hooks")
			if flagJSON {
				return emitJSON(hooksCfg)
			}
			if len(hooksCfg) == 0 {
				fmt.Println("No hooks configured.")
				return nil
			}
			for _, event := range sortedKeys(hooksCfg) {
				fmt.Printf("\n  %s:\n", event)
				for _, hv := range asSlice(hooksCfg[event]) {
					hm := asMap(hv)
					name := strOr(str(hm["name"]), "(unnamed)")
					fmt.Printf("    - %s: %s\n", name, strOr(str(hm["type"]), "shell"))
				}
			}
			return nil
		}}

	var limit int
	logCmd := &cobra.Command{Use: "log", Short: "Show recent hook execution log", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				return fmt.Errorf("--limit must be positive")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id, hook_name, event_id, status, COALESCE(response,''), fired_at FROM hook_log ORDER BY fired_at DESC LIMIT ?`, limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			type logRow struct {
				ID, HookName, EventType, Status, Response, FiredAt string
			}
			var out []logRow
			for rows.Next() {
				var r logRow
				rows.Scan(&r.ID, &r.HookName, &r.EventType, &r.Status, &r.Response, &r.FiredAt)
				out = append(out, r)
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No hook log entries.")
				return nil
			}
			fmt.Printf("%-14s %-20s %-25s %-8s %s\n", "ID", "Event", "Hook", "Status", "Time")
			fmt.Println(strings.Repeat("-", 90))
			for _, r := range out {
				fmt.Printf("  %-12s %-20s %-25s %-8s %s\n", r.ID, r.EventType, r.HookName, r.Status, r.FiredAt)
			}
			return nil
		}}
	logCmd.Flags().IntVar(&limit, "limit", 50, "max log rows")

	test := &cobra.Command{Use: "test <event>", Short: "Test-fire hooks for an event type", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			event := args[0]
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			payload := map[string]string{
				"event_type": event, "test": "true",
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			}
			hooksCfg := app.Cfg.Section("hooks")
			hookList := asSlice(hooksCfg[event])
			fired := 0
			for _, hv := range hookList {
				hm := asMap(hv)
				ok, errMsg := executeHook(hm, payload)
				if ok {
					fired++
				}
				status := "ok"
				if !ok {
					status = "error"
				}
				var resp any
				if errMsg != "" {
					resp = errMsg
				}
				db.Exec(`INSERT INTO hook_log(id,hook_name,event_id,status,response,fired_at) VALUES(?,?,?,?,?,?)`,
					uuid.NewString()[:12], str(hm["name"]), event, status, resp, time.Now().UTC().Format(time.RFC3339))
			}
			if fired == 0 {
				fmt.Printf("⚠ No hooks matched event '%s'\n", event)
			}
			fmt.Printf("Fired %d hook(s) for event '%s'\n", fired, event)
			return nil
		}}

	h.AddCommand(list, logCmd, test)
	root.AddCommand(h)
}

// executeHook runs a single shell hook with shell-safe interpolation and a 30s
// timeout. Returns (ok, errMsg). Non-shell (e.g. webhook) types are not fired.
func executeHook(hook map[string]any, payload map[string]string) (bool, string) {
	hookType := strOr(str(hook["type"]), "shell")
	if hookType != "shell" {
		return false, fmt.Sprintf("hook type %q not supported in native runtime", hookType)
	}
	cmdStr := interpolateShell(str(hook["command"]), payload)
	if strings.TrimSpace(cmdStr) == "" {
		return false, "empty command"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	if err := c.Run(); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// interpolateShell substitutes {{var}} with shell-quoted payload values so
// attacker-influenced data can't break out of its argument (port of _interpolate
// with shell_safe=True).
func interpolateShell(tmpl string, payload map[string]string) string {
	out := tmpl
	for k, v := range payload {
		out = strings.ReplaceAll(out, "{{"+k+"}}", shellQuote(v))
	}
	return out
}

// shellQuote wraps s in single quotes, escaping embedded single quotes (shlex.quote).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
