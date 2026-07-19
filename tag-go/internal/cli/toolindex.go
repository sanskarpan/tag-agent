package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
)

// registerToolIndex wires tool retrieval over the MCP registry: tool-index
// index/search/status. Port of src/tag/cmd/agent_tools.py:cmd_tool_index using
// the keyword-search fallback (the vector backend needs Python-only deps; the
// keyword path is faithful and dependency-free).
func registerToolIndex(root *cobra.Command, app *App) {
	t := &cobra.Command{Use: "tool-index", Short: "Tool retrieval over the MCP registry", GroupID: "tools"}

	index := &cobra.Command{Use: "index", Short: "Build the tool index from the MCP registry", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			servers, err := config.MCPRegistry()
			if err != nil {
				return err
			}
			if _, err := db.Exec(`DELETE FROM tool_index`); err != nil {
				return err
			}
			count := 0
			for _, sname := range sortedKeys(servers) {
				info := asMap(servers[sname])
				if _, err := db.Exec(`INSERT OR REPLACE INTO tool_index(name,description,server) VALUES(?,?,?)`,
					sname, str(info["description"]), sname); err != nil {
					return err
				}
				count++
			}
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := db.Exec(`INSERT OR REPLACE INTO tool_index_meta(id,tool_count,built_at) VALUES('singleton',?,?)`, count, now); err != nil {
				return err
			}
			fmt.Printf("✓ Tool index built: %d tools indexed\n", count)
			return nil
		}}

	var topK int
	search := &cobra.Command{Use: "search <query>", Short: "Search tools by keyword", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.TrimSpace(args[0])
			if query == "" {
				return fmt.Errorf("query must not be empty")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT name, description, server FROM tool_index`)
			if err != nil {
				return err
			}
			type tool struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Server      string `json:"server"`
				score       int
			}
			var tools []tool
			for rows.Next() {
				var tl tool
				if err := rows.Scan(&tl.Name, &tl.Description, &tl.Server); err != nil {
					continue
				}
				tools = append(tools, tl)
			}
			rows.Close()
			// keyword score: number of query terms appearing in name+description
			terms := strings.Fields(strings.ToLower(query))
			var scored []tool
			for _, tl := range tools {
				text := strings.ToLower(tl.Name + " " + tl.Description)
				s := 0
				for _, w := range terms {
					if strings.Contains(text, w) {
						s++
					}
				}
				if s > 0 {
					tl.score = s
					scored = append(scored, tl)
				}
			}
			sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
			if topK > 0 && len(scored) > topK {
				scored = scored[:topK]
			}
			if flagJSON {
				return emitJSON(scored)
			}
			if len(scored) == 0 {
				fmt.Printf("No tools found for query: %q\n", query)
				return nil
			}
			fmt.Printf("Top %d tools for: %q\n\n", len(scored), query)
			for i, tl := range scored {
				fmt.Printf("  %2d. [%-20s] %-30s  %s\n", i+1, tl.Server, tl.Name, truncate(tl.Description, 60))
			}
			return nil
		}}
	search.Flags().IntVar(&topK, "top-k", 8, "max results")

	status := &cobra.Command{Use: "status", Short: "Show tool index status", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var count int
			var builtAt string
			err = db.QueryRow(`SELECT tool_count, built_at FROM tool_index_meta WHERE id='singleton'`).Scan(&count, &builtAt)
			if err != nil {
				if flagJSON {
					return emitJSON(map[string]any{"built": false, "tool_count": 0})
				}
				fmt.Println("Tool index not built. Run: tag tool-index index")
				return nil
			}
			if flagJSON {
				return emitJSON(map[string]any{"built": true, "tool_count": count, "built_at": builtAt, "backend": "keyword"})
			}
			fmt.Printf("Index status:  %d tools\n", count)
			fmt.Printf("Built at:      %s\n", builtAt)
			fmt.Printf("Backend:       keyword\n")
			return nil
		}}

	t.AddCommand(index, search, status)
	root.AddCommand(t)
}
