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
			// out[:limit] panicked with a Go stack trace on a negative value:
			//   panic: runtime error: slice bounds out of range [:-1]
			if limit < 0 {
				return usageErrorf("--limit must be >= 0 (got %d)", limit)
			}
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
			// Mixed timestamp precision, normalised before sorting.
			//
			// runs.created_at is second-precision RFC3339 ("...:26Z") while
			// spans.started_at is microsecond ("...:26.739020Z"). Sorting those
			// lexicographically puts 'Z' (0x5A) above '.' (0x2E) at index 19, so
			// EVERY run sorted above EVERY span regardless of real time and this
			// command returned the OLDEST rows — the reverse of a tail.
			sort.SliceStable(out, func(i, j int) bool {
				return normalizeTS(out[i].Timestamp) > normalizeTS(out[j].Timestamp)
			})
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

// normalizeTS pads a second-precision RFC3339 stamp with a zero fraction so it
// compares correctly against a microsecond one.
func normalizeTS(ts string) string {
	if i := strings.IndexByte(ts, '.'); i >= 0 {
		return ts
	}
	if len(ts) >= 20 && ts[19] == 'Z' {
		return ts[:19] + ".000000Z" + ts[20:]
	}
	return ts
}
