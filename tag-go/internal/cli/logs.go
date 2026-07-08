package cli

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// registerLogs wires `tag logs`, a read-only tail of recent structured
// activity. Port of the intent behind src/tag/cmd/session.py:cmd_logs (which
// shells out to hermes); the Go build reads directly from the state store,
// unioning the `runs` table with the `spans` table when present, newest first.
func registerLogs(root *cobra.Command, app *App) {
	var limit int
	c := &cobra.Command{Use: "logs", Short: "Tail recent activity (runs + spans)", GroupID: "obs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			type event struct {
				Source    string `json:"source"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Status    string `json:"status"`
				Profile   string `json:"profile"`
				Model     string `json:"model_id"`
				Timestamp string `json:"timestamp"`
			}
			out := []event{}

			// runs: prompt stands in for the event name.
			rrows, err := db.Query(`SELECT id, prompt, status, master_profile, COALESCE(model_id,''), created_at
				FROM runs ORDER BY created_at DESC LIMIT ?`, limit)
			if err != nil {
				return err
			}
			for rrows.Next() {
				var e event
				e.Source = "run"
				if err := rrows.Scan(&e.ID, &e.Name, &e.Status, &e.Profile, &e.Model, &e.Timestamp); err != nil {
					rrows.Close()
					return err
				}
				e.Name = oneLine(e.Name)
				out = append(out, e)
			}
			rrows.Close()

			// spans: present only if the table exists (tolerate absence).
			srows, serr := db.Query(`SELECT id, name, status, COALESCE(profile,''), COALESCE(model_id,''),
				COALESCE(finished_at, started_at) FROM spans ORDER BY started_at DESC LIMIT ?`, limit)
			if serr == nil {
				for srows.Next() {
					var e event
					e.Source = "span"
					var ts sql.NullString
					if err := srows.Scan(&e.ID, &e.Name, &e.Status, &e.Profile, &e.Model, &ts); err != nil {
						srows.Close()
						return err
					}
					e.Timestamp = ts.String
					out = append(out, e)
				}
				srows.Close()
			}

			// Merge newest-first across both sources, then cap at limit.
			// ISO timestamps sort lexicographically; stable to keep source order on ties.
			sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
			if len(out) > limit {
				out = out[:limit]
			}

			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No activity found.")
				return nil
			}
			fmt.Printf("%-6s %-14s %-40s %-10s %-16s %s\n", "Source", "ID", "Event", "Status", "Profile", "When")
			fmt.Println(strings.Repeat("-", 110))
			for _, e := range out {
				fmt.Printf("%-6s %-14s %-40s %-10s %-16s %s\n",
					e.Source, truncate(e.ID, 14), truncate(e.Name, 40), truncate(e.Status, 10),
					truncate(e.Profile, 16), e.Timestamp)
			}
			return nil
		}}
	c.Flags().IntVar(&limit, "limit", 20, "max events to return")
	root.AddCommand(c)
}
