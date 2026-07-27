package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/graph"
	"github.com/tag-agent/tag/internal/memory"
)

// registerGraph wires the entity knowledge graph: graph show/query/build.
// Port of src/tag/cmd/prd_clusters.py:cmd_entity_graph + entity_graph.py.
func registerGraph(root *cobra.Command, app *App) {
	g := &cobra.Command{Use: "graph", Short: "Entity knowledge graph", GroupID: "memory"}
	var profile string

	show := &cobra.Command{Use: "show", Short: "Show entity graph summary", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			p := app.profile(profile)
			nEnt, nRel, nComm, err := graph.Summary(db, p)
			if err != nil {
				return err
			}
			if flagJSON {
				ents, err := graph.Query(db, p, "", 50)
				if err != nil {
					return err
				}
				if ents == nil {
					ents = []graph.Entity{} // emit [] not null (parity, issue #528)
				}
				rels, err := graph.Relations(db, p, nil)
				if err != nil {
					return err
				}
				return emitJSON(map[string]any{"entities": ents, "relations": rels, "counts": map[string]int{
					"entities": nEnt, "relations": nRel, "communities": nComm}})
			}
			fmt.Printf("%d entities, %d relations, %d communities\n", nEnt, nRel, nComm)
			return nil
		}}

	var depth int
	query := &cobra.Command{Use: "query <entity>", Short: "Query graph by entity name", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			ents, err := graph.Query(db, app.profile(profile), args[0], 50)
			if err != nil {
				return err
			}
			if flagJSON {
				if ents == nil {
					ents = []graph.Entity{} // emit [] not null (parity, issue #534)
				}
				return emitJSON(map[string]any{"entities": ents})
			}
			if len(ents) == 0 {
				fmt.Printf("No entities matching %q\n", args[0])
				return nil
			}
			fmt.Printf("%d entities matching %q (depth %d):\n", len(ents), args[0], depth)
			for _, e := range ents {
				fmt.Printf("  %s (%s) mentions=%d\n", e.Name, e.EntityType, e.MentionCount)
			}
			return nil
		}}
	query.Flags().IntVar(&depth, "depth", 2, "neighborhood depth")

	build := &cobra.Command{Use: "build", Short: "Build graph from existing memories", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			p := app.profile(profile)
			// Read the source memories BEFORE opening the write transaction: the
			// store pins MaxOpenConns(1), so a query issued on db while a tx holds
			// that single connection would deadlock.
			mems, err := memory.List(db.DB, p, "", 10000)
			if err != nil {
				return err
			}
			// Reset + rebuild must be ONE transaction. Run as separate autocommit
			// statements, a concurrent `graph build` could observe the half-cleared
			// graph and re-extract on top of it, inflating mention_count. The first
			// statement inside the tx is Reset's DELETE, so SQLite takes the write
			// lock up front (no read-then-upgrade deadlock) and builds serialize.
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded
			// Idempotent rebuild: clear prior state so mention_count isn't re-inflated (C021).
			if err := graph.Reset(tx, p); err != nil {
				return err
			}
			entCount, relCount := 0, 0
			for _, m := range mems {
				e, r, err := graph.ExtractAndStore(tx, m.ID, m.Content, p)
				if err != nil {
					return err
				}
				entCount += e
				relCount += r
			}
			comms, err := graph.DetectCommunities(tx, p)
			if err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			outJSON(map[string]any{"memories": len(mems), "entities": entCount, "relations": relCount, "communities": len(comms)},
				fmt.Sprintf("Built graph from %d memories: %d entities, %d relations, %d communities",
					len(mems), entCount, relCount, len(comms)))
			return nil
		}}

	g.PersistentFlags().StringVar(&profile, "profile", "", "profile")
	g.AddCommand(show, query, build)
	root.AddCommand(g)
}
