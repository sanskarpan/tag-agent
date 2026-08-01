package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/memory"
	"github.com/tag-agent/tag/internal/toolindex"
)

// registerToolIndex wires tool retrieval over the MCP registry: tool-index
// index/search/status. Port of src/tag/cmd/agent_tools.py:cmd_tool_index.
//
// PRD-043: retrieval is vector/embedding-based when an embeddings backend is
// configured (TAG_EMBED_BASE_URL / TAG_EMBED_API_KEY, falling back to
// OPENAI_API_KEY) — the same machinery `mem2 store search` uses — and degrades
// to the original keyword scan otherwise. Every command reports which backend
// actually ran, so a keyword result is never mistaken for a semantic one.
func registerToolIndex(root *cobra.Command, app *App) {
	t := &cobra.Command{Use: "tool-index", Short: "Tool retrieval over the MCP registry (vector when configured, keyword otherwise)", GroupID: "tools"}

	// embedderOrNil resolves the embeddings backend from the environment. It must
	// stay a nil *interface* when unconfigured: boxing a nil *OpenAIEmbedder would
	// defeat the e==nil guards downstream.
	embedderOrNil := func() memory.Embedder { return memory.EmbedderFromEnvIface() }

	var noEmbed bool
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
			var tools []toolindex.Tool
			for _, sname := range sortedKeys(servers) {
				info := asMap(servers[sname])
				tools = append(tools, toolindex.Tool{Name: sname, Description: str(info["description"]), Server: sname})
			}
			// Embeddings cost money, so they are only computed when the user has
			// configured a backend (an explicit action) and has not opted out.
			var e memory.Embedder
			if !noEmbed {
				e = embedderOrNil()
			}
			res, err := toolindex.Build(cmd.Context(), db.DB, e, tools)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(res)
			}
			fmt.Printf("✓ Tool index built: %d tools indexed (backend: %s)\n", res.Tools, res.Mode)
			if res.Embedded > 0 {
				fmt.Printf("  embedded %d tool documents with %s\n", res.Embedded, res.Model)
			}
			if res.Note != "" {
				fmt.Fprintf(os.Stderr, "  note: %s\n", res.Note)
			}
			return nil
		}}
	index.Flags().BoolVar(&noEmbed, "no-embed", false, "skip embedding even when a backend is configured (keyword index only)")

	var topK int
	search := &cobra.Command{Use: "search <query>", Short: "Search tools (vector when indexed with embeddings, keyword otherwise)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			hits, mode, err := toolindex.Search(cmd.Context(), db.DB, embedderOrNil(), args[0], topK)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if hits == nil {
				hits = []toolindex.Tool{}
			}
			if flagJSON {
				// The retrieval mode travels with the payload (same contract as
				// `mem2 store search --json`), so a consumer can tell a semantic
				// ranking from the keyword fallback without guessing.
				return emitJSON(map[string]any{"mode": mode, "query": args[0], "results": hits})
			}
			if len(hits) == 0 {
				fmt.Printf("No tools found for query: %q (mode: %s)\n", args[0], mode)
				return nil
			}
			fmt.Printf("Top %d tools for: %q  (mode: %s)\n\n", len(hits), args[0], mode)
			for i, tl := range hits {
				fmt.Printf("  %2d. [%-20s] %-30s  %.4f  %s\n", i+1, tl.Server, tl.Name, tl.Score, truncate(tl.Description, 60))
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
			st, err := toolindex.Load(db.DB, embedderOrNil())
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(st)
			}
			if !st.Built {
				fmt.Println("Tool index not built. Run: tag tool-index index")
				return nil
			}
			fmt.Printf("Index status:  %d tools\n", st.ToolCount)
			fmt.Printf("Built at:      %s\n", st.BuiltAt)
			fmt.Printf("Backend:       %s\n", st.Backend)
			fmt.Printf("Vectors:       %d", st.Vectors)
			if st.EmbedModel != "" {
				fmt.Printf(" (%s)", st.EmbedModel)
			}
			fmt.Println()
			if !st.VectorReady {
				fmt.Println("Vector retrieval unavailable — configure TAG_EMBED_BASE_URL or OPENAI_API_KEY and re-run 'tag tool-index index'.")
			}
			return nil
		}}

	t.AddCommand(index, search, status)
	root.AddCommand(t)
}
