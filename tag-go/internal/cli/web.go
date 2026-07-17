package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/server"
)

// registerWeb wires `tag web` — the local web dashboard (PRD-036): HTML
// dashboard + JSON endpoints for runs, per-run span waterfalls, queue, and cost
// summaries, with an SSE live feed. Bound to loopback. Port of cmd_web.
func registerWeb(root *cobra.Command, app *App) {
	var webPort int
	var webHost string
	c := &cobra.Command{
		Use:     "web",
		Short:   "Start the local web dashboard server (SSE)",
		GroupID: "obs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if webHost != "" && webHost != "127.0.0.1" && webHost != "localhost" {
				fmt.Printf("warning: --host %s ignored; the dashboard binds to loopback only\n", webHost)
			}
			return server.ServeWeb(db, webPort)
		},
	}
	c.Flags().IntVar(&webPort, "port", 8787, "port to listen on")
	c.Flags().StringVar(&webHost, "host", "127.0.0.1", "host to bind (loopback only)")
	root.AddCommand(c)
}
