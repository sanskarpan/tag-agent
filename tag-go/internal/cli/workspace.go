package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// wsRenderTree renders a nested file tree (mirrors workspace._render_tree).
// A leaf is represented by a nil map value.
func wsRenderTree(node map[string]any, lines *[]string, prefix string) {
	type kv struct {
		name  string
		child any
	}
	items := make([]kv, 0, len(node))
	for k, v := range node {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		iLeaf := items[i].child == nil
		jLeaf := items[j].child == nil
		if iLeaf != jLeaf {
			return !iLeaf // dirs (false) before leaves (true)
		}
		return items[i].name < items[j].name
	})
	for i, it := range items {
		connector := "├── "
		ext := "│   "
		if i == len(items)-1 {
			connector = "└── "
			ext = "    "
		}
		*lines = append(*lines, prefix+connector+it.name)
		if child, ok := it.child.(map[string]any); ok {
			wsRenderTree(child, lines, prefix+ext)
		}
	}
}

var wsIncludeExt = map[string]bool{".go": true, ".py": true, ".js": true, ".ts": true, ".md": true, ".rs": true, ".java": true, ".c": true, ".h": true, ".rb": true, ".yaml": true, ".yml": true, ".json": true, ".toml": true}
var wsSkipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "__pycache__": true, ".venv": true, "dist": true, "build": true}

func registerWorkspace(root *cobra.Command, app *App) {
	var path string
	var maxFiles int
	ws := &cobra.Command{Use: "workspace", Short: "Repo-map / workspace context", GroupID: "tools"}
	index := &cobra.Command{Use: "index", Short: "Index the workspace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				path = "."
			}
			abs, _ := filepath.Abs(path)
			st, err := os.Stat(abs)
			if err != nil || !st.IsDir() {
				return fmt.Errorf("not a directory: %s", abs)
			}
			if maxFiles <= 0 {
				return fmt.Errorf("max-files must be > 0")
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			// Two-phase, mirroring workspace.index_workspace: first score every
			// candidate by (recency + inverse-size), then keep the top max_files by
			// rank. A constant rank made `workspace map`'s ORDER BY rank DESC a no-op.
			type wsCand struct {
				path string
				rank float64
			}
			var cands []wsCand
			now := time.Now()
			_ = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if wsSkipDirs[d.Name()] {
						return filepath.SkipDir
					}
					return nil
				}
				if !wsIncludeExt[strings.ToLower(filepath.Ext(p))] {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				size := float64(info.Size())
				// Rank: recency weight + inverse-size weight (smaller = easier to
				// include). Matches workspace.py exactly.
				ageDays := now.Sub(info.ModTime()).Seconds() / 86400.0
				rank := 1.0/(1.0+ageDays*0.1) + 1.0/(1.0+size/10000.0)
				cands = append(cands, wsCand{p, rank})
				return nil
			})
			sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank > cands[j].rank })
			if len(cands) > maxFiles {
				cands = cands[:maxFiles]
			}
			indexed, tokens := 0, 0
			nowISO := time.Now().UTC().Format(time.RFC3339)
			for _, c := range cands {
				b, err := os.ReadFile(c.path)
				if err != nil {
					continue
				}
				h := sha256.Sum256(b)
				tok := len(b) / 4
				rel, _ := filepath.Rel(abs, c.path)
				if _, err := db.Exec(`INSERT INTO workspace_files(path,content_hash,byte_size,token_count,rank,indexed_at) VALUES(?,?,?,?,?,?)
					ON CONFLICT(path) DO UPDATE SET content_hash=excluded.content_hash,byte_size=excluded.byte_size,token_count=excluded.token_count,rank=excluded.rank,indexed_at=excluded.indexed_at`,
					rel, hex.EncodeToString(h[:8]), len(b), tok, c.rank, nowISO); err != nil {
					return err
				}
				indexed++
				tokens += tok
			}
			outJSON(map[string]any{"files_indexed": indexed, "total_tokens": tokens}, fmt.Sprintf("Indexed %d files (%d tokens)", indexed, tokens))
			return nil
		}}
	index.Flags().StringVar(&path, "path", ".", "path")
	index.Flags().IntVar(&maxFiles, "max-files", 500, "max files")
	status := &cobra.Command{Use: "status", Short: "Show index status", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var n, tok int
			if err := db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(token_count),0) FROM workspace_files`).Scan(&n, &tok); err != nil {
				return err
			}
			outJSON(map[string]any{"file_count": n, "total_tokens": tok}, fmt.Sprintf("Indexed: %d files  %d tokens", n, tok))
			return nil
		}}

	var budget int
	mapCmd := &cobra.Command{Use: "map", Short: "Render a token-efficient repo map", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				path = "."
			}
			abs, _ := filepath.Abs(path)
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT path, token_count FROM workspace_files ORDER BY rank DESC LIMIT 200`)
			if err != nil {
				return err
			}
			defer rows.Close()
			tree := map[string]any{}
			included := 0
			hasRows := false
			for rows.Next() {
				hasRows = true
				var p string
				var tok int
				if err := rows.Scan(&p, &tok); err != nil {
					return err
				}
				if included+tok > budget {
					continue
				}
				included += tok
				parts := strings.Split(filepath.ToSlash(p), "/")
				node := tree
				for _, part := range parts[:len(parts)-1] {
					child, ok := node[part].(map[string]any)
					if !ok {
						child = map[string]any{}
						node[part] = child
					}
					node = child
				}
				node[parts[len(parts)-1]] = nil
			}
			if err := rows.Err(); err != nil {
				return err
			}
			var out string
			if !hasRows {
				out = "(workspace not indexed — run `tag workspace index` first)"
			} else {
				lines := []string{fmt.Sprintf("Workspace: %s/  (%d tokens)", filepath.Base(abs), included)}
				wsRenderTree(tree, &lines, "")
				out = strings.Join(lines, "\n")
			}
			outJSON(map[string]any{"map": out}, out)
			return nil
		}}
	mapCmd.Flags().StringVar(&path, "path", ".", "path")
	mapCmd.Flags().IntVar(&budget, "budget", 4000, "token budget")

	clear := &cobra.Command{Use: "clear", Short: "Clear the workspace index", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if _, err := db.Exec(`DELETE FROM workspace_files`); err != nil {
				return err
			}
			fmt.Println("Workspace index cleared.")
			return nil
		}}

	ws.AddCommand(index, status, mapCmd, clear)
	root.AddCommand(ws)
}
