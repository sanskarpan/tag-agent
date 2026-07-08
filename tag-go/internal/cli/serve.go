package cli

import (
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/server"
)

// registerServe wires `tag serve` — the local HTTP dashboard + JSON snapshot API
// + SSE event stream (Track B, PRD-029). Bound to loopback. Port of cmd_serve.
func registerServe(root *cobra.Command, app *App) {
	var port int
	var profile string
	c := &cobra.Command{
		Use:     "serve",
		Short:   "Start the local HTTP dashboard server (SSE)",
		GroupID: "obs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			return server.Serve(db, app.profile(profile), port)
		},
	}
	c.Flags().IntVar(&port, "port", 7880, "port to listen on")
	c.Flags().StringVar(&profile, "profile", "", "default profile for the dashboard")
	root.AddCommand(c)
}
