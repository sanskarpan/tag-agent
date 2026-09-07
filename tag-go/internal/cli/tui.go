package cli

import (
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/tui"
)

// registerTUI wires `tag tui` — the interactive Charm terminal dashboard.
func registerTUI(root *cobra.Command, app *App) {
	var profile string
	c := &cobra.Command{
		Use:     "tui",
		Short:   "Interactive terminal dashboard",
		GroupID: "obs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			selected := app.profile(profile)
			if err := ensureProfileExists(app.Cfg, selected); err != nil {
				return err
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			return tui.Run(db, selected)
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "profile to view")
	root.AddCommand(c)
}
