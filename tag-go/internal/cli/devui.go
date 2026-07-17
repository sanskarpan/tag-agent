package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/server"
)

// registerDevui wires `tag devui` — a richer local developer dashboard
// (PRD-054): HTML dashboard + JSON endpoints for spans/traces, evals, memories,
// alerts, and aggregate stats. Bound to loopback. Port of cmd_devui.
func registerDevui(root *cobra.Command, app *App) {
	var dvPort int
	var dvProfile string
	var dvOpen bool
	c := &cobra.Command{
		Use:     "devui",
		Short:   "Start the local DevUI developer dashboard",
		GroupID: "obs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if dvOpen {
				fmt.Printf("Open http://127.0.0.1:%d in your browser\n", dvPort)
			}
			return server.ServeDevUI(db, app.profile(dvProfile), dvPort)
		},
	}
	c.Flags().IntVar(&dvPort, "port", 7777, "port to listen on")
	c.Flags().StringVar(&dvProfile, "profile", "", "default profile for the dashboard")
	c.Flags().BoolVar(&dvOpen, "open", false, "print the dashboard URL to open in a browser")
	root.AddCommand(c)
}
