package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/webhook"
)

// registerWebhook wires the CI/CD webhook receiver: webhook listen/rule-add/rule-list/events.
// Port of src/tag/cmd/prd_clusters.py:cmd_webhook_server + webhook_server.py.
func registerWebhook(root *cobra.Command, app *App) {
	w := &cobra.Command{Use: "webhook", Short: "Webhook server for CI/CD automation", GroupID: "obs"}

	var host, secret, profile string
	var port int
	var allowUnsigned bool
	listen := &cobra.Command{Use: "listen", Short: "Start the webhook server", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			sec := secret
			if sec == "" {
				sec = os.Getenv("TAG_WEBHOOK_SECRET")
			}
			if sec == "" && !allowUnsigned {
				return fmt.Errorf("refusing to start without an HMAC secret: set --secret or TAG_WEBHOOK_SECRET, or pass --allow-unsigned to accept unauthenticated events")
			}
			return webhook.Serve(db, strOr(host, "127.0.0.1"), port, sec, allowUnsigned)
		}}
	listen.Flags().StringVar(&host, "host", "127.0.0.1", "bind host")
	listen.Flags().IntVar(&port, "port", 8765, "bind port")
	listen.Flags().StringVar(&secret, "secret", "", "HMAC secret (or TAG_WEBHOOK_SECRET)")
	listen.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "accept unauthenticated events when no secret is set (INSECURE)")
	listen.Flags().StringVar(&profile, "profile", "", "profile")

	var rPlatform, rEvent, rProfile, rAction string
	ruleAdd := &cobra.Command{Use: "rule-add", Short: "Add a trigger rule", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if rPlatform == "" || rEvent == "" || rProfile == "" {
				return fmt.Errorf("--platform, --event, and --profile are required")
			}
			switch rPlatform {
			case "github", "linear", "slack":
			default:
				return fmt.Errorf("--platform must be github|linear|slack")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rule, err := webhook.CreateRule(db, rPlatform, rEvent, rProfile, strOr(rAction, "run"), nil)
			if err != nil {
				return err
			}
			fmt.Printf("Added trigger rule %s: %s %s -> %s (%s)\n", rule.ID, rPlatform, rEvent, rProfile, rule.Action)
			return nil
		}}
	ruleAdd.Flags().StringVar(&rPlatform, "platform", "", "github|linear|slack")
	ruleAdd.Flags().StringVar(&rEvent, "event", "", "event pattern (supports globs)")
	ruleAdd.Flags().StringVar(&rProfile, "profile", "", "profile to run")
	ruleAdd.Flags().StringVar(&rAction, "action", "run", "action")

	ruleList := &cobra.Command{Use: "rule-list", Short: "List trigger rules", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rules, err := webhook.ListRules(db, "")
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(rules)
			}
			if len(rules) == 0 {
				fmt.Println("No trigger rules configured.")
				return nil
			}
			for _, r := range rules {
				fmt.Printf("  %s  %-8s %-24s -> %-14s (%s)\n", r.ID, r.Platform, r.Event, r.Profile, r.Action)
			}
			return nil
		}}

	var limit int
	events := &cobra.Command{Use: "events", Short: "List recent webhook events", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT id, platform, event_type, signature_valid, status, received_at FROM webhook_events ORDER BY received_at DESC LIMIT ?`, limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			type ev struct {
				ID, Platform, EventType, Status, ReceivedAt string
				SignatureValid                              bool
			}
			out := []ev{}
			for rows.Next() {
				var e ev
				var sv int
				if err := rows.Scan(&e.ID, &e.Platform, &e.EventType, &sv, &e.Status, &e.ReceivedAt); err != nil {
					return err
				}
				e.SignatureValid = sv != 0
				out = append(out, e)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No webhook events recorded.")
				return nil
			}
			for _, e := range out {
				fmt.Printf("  %s  %-8s %-22s valid=%v  %s\n", e.ID, e.Platform, e.EventType, e.SignatureValid, e.Status)
			}
			return nil
		}}
	events.Flags().IntVar(&limit, "limit", 20, "max events")

	w.AddCommand(listen, ruleAdd, ruleList, events)
	root.AddCommand(w)
}
