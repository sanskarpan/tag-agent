package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/store"
)

// registerPrompt wires the prompt versioning hub: prompt save/get/list/versions/diff.
// Port of src/tag/cmd/prd_clusters.py:cmd_prompt_hub + prompt_hub.py.
func registerPrompt(root *cobra.Command, app *App) {
	p := &cobra.Command{Use: "prompt", Short: "Prompt versioning hub", GroupID: "tools"}

	var notes string
	save := &cobra.Command{Use: "save <name> <file>", Short: "Save a new prompt version from a file", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return fmt.Errorf("prompt name must not be empty or blank")
			}
			content, err := os.ReadFile(args[1])
			if err != nil {
				return fmt.Errorf("prompt file not found: %s", args[1])
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var maxVer int
			db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM prompt_versions WHERE name=?`, name).Scan(&maxVer)
			newVer := maxVer + 1
			vars := extractPromptVars(string(content))
			varsJSON, _ := json.Marshal(vars)
			sum := sha256.Sum256(content)
			id := uuid.NewString()
			_, err = db.Exec(`INSERT INTO prompt_versions(id,name,version,content,variables_json,tags_json,parent_version_id,author,message,sha256,created_at,is_active)
				VALUES(?,?,?,?,?,'[]',NULL,NULL,?,?,?,1)`,
				id, name, newVer, string(content), string(varsJSON), notes, hex.EncodeToString(sum[:]), time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			fmt.Printf("Saved '%s' v%d (id=%s)\n", name, newVer, id)
			return nil
		}}
	save.Flags().StringVar(&notes, "notes", "", "commit message for this version")

	var version int
	get := &cobra.Command{Use: "get <name>", Short: "Print latest (or --version) prompt content", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			content, ok, err := getPromptContent(app, db, args[0], version, cmd.Flags().Changed("version"))
			if err != nil {
				return err
			}
			if !ok {
				if cmd.Flags().Changed("version") {
					// distinguish missing version from missing prompt (C043)
					if _, exists, _ := getPromptContent(app, db, args[0], 0, false); exists {
						return jsonErrorMaybe(fmt.Errorf("prompt %q version %d not found", args[0], version))
					}
				}
				return jsonErrorMaybe(fmt.Errorf("prompt not found: %q", args[0]))
			}
			fmt.Println(content)
			return nil
		}}
	get.Flags().IntVar(&version, "version", 0, "specific version (default latest active)")

	list := &cobra.Command{Use: "list", Short: "List saved prompts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT name, MAX(version), COUNT(*) FROM prompt_versions GROUP BY name ORDER BY name`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type promptSummary struct {
				Name          string `json:"name"`
				LatestVersion int    `json:"latest_version"`
				VersionsCount int    `json:"versions_count"`
			}
			var out []promptSummary
			for rows.Next() {
				var s promptSummary
				if err := rows.Scan(&s.Name, &s.LatestVersion, &s.VersionsCount); err != nil {
					return err
				}
				out = append(out, s)
			}
			if flagJSON {
				if out == nil {
					out = []promptSummary{}
				}
				return emitJSON(out)
			}
			for _, s := range out {
				fmt.Printf("%-40s v%d (%d versions)\n", s.Name, s.LatestVersion, s.VersionsCount)
			}
			return nil
		}}

	versions := &cobra.Command{Use: "versions <name>", Short: "List all versions of a prompt", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT version, sha256, created_at, COALESCE(message,'') FROM prompt_versions WHERE name=? ORDER BY version ASC`, args[0])
			if err != nil {
				return err
			}
			defer rows.Close()
			type promptVersion struct {
				Version   int    `json:"version"`
				SHA256    string `json:"sha256"`
				CreatedAt string `json:"created_at"`
				Message   string `json:"message"`
			}
			var out []promptVersion
			for rows.Next() {
				var pv promptVersion
				if err := rows.Scan(&pv.Version, &pv.SHA256, &pv.CreatedAt, &pv.Message); err != nil {
					return err
				}
				out = append(out, pv)
			}
			if len(out) == 0 {
				return jsonErrorMaybe(fmt.Errorf("prompt not found: %q", args[0]))
			}
			if flagJSON {
				return emitJSON(out)
			}
			for _, pv := range out {
				fmt.Printf("v%-3d %s  %s  %s\n", pv.Version, pv.SHA256[:12], pv.CreatedAt, pv.Message)
			}
			return nil
		}}

	diff := &cobra.Command{Use: "diff <name> <v1> <v2>", Short: "Unified diff between two versions", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			var v1, v2 int
			if _, err := fmt.Sscan(args[1], &v1); err != nil {
				return jsonErrorMaybe(fmt.Errorf("invalid version %q", args[1]))
			}
			if _, err := fmt.Sscan(args[2], &v2); err != nil {
				return jsonErrorMaybe(fmt.Errorf("invalid version %q", args[2]))
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			c1, ok1, _ := getPromptContent(app, db, args[0], v1, true)
			if !ok1 {
				return jsonErrorMaybe(fmt.Errorf("prompt %q version %d not found", args[0], v1))
			}
			c2, ok2, _ := getPromptContent(app, db, args[0], v2, true)
			if !ok2 {
				return jsonErrorMaybe(fmt.Errorf("prompt %q version %d not found", args[0], v2))
			}
			fmt.Print(unifiedDiff(c1, c2, fmt.Sprintf("%s v%d", args[0], v1), fmt.Sprintf("%s v%d", args[0], v2)))
			return nil
		}}

	p.AddCommand(save, get, list, versions, diff)
	root.AddCommand(p)
}

var promptVarRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// extractPromptVars returns {{var}} placeholders in first-appearance order, deduped.
func extractPromptVars(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range promptVarRe.FindAllStringSubmatch(content, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// getPromptContent returns (content, found, err). If pinned is false, returns the
// latest active version; otherwise the exact version.
func getPromptContent(app *App, db *store.DB, name string, version int, pinned bool) (string, bool, error) {
	var content string
	var err error
	if pinned {
		err = db.QueryRow(`SELECT content FROM prompt_versions WHERE name=? AND version=?`, name, version).Scan(&content)
	} else {
		err = db.QueryRow(`SELECT content FROM prompt_versions WHERE name=? AND is_active=1 ORDER BY version DESC LIMIT 1`, name).Scan(&content)
	}
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// unifiedDiff produces a compact LCS-based unified diff between two texts.
func unifiedDiff(a, b, fromLabel, toLabel string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	// LCS table
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", fromLabel, toLabel)
	i, j := 0, 0
	for i < n && j < m {
		if al[i] == bl[j] {
			fmt.Fprintf(&sb, " %s\n", al[i])
			i++
			j++
		} else if lcs[i+1][j] >= lcs[i][j+1] {
			fmt.Fprintf(&sb, "-%s\n", al[i])
			i++
		} else {
			fmt.Fprintf(&sb, "+%s\n", bl[j])
			j++
		}
	}
	for ; i < n; i++ {
		fmt.Fprintf(&sb, "-%s\n", al[i])
	}
	for ; j < m; j++ {
		fmt.Fprintf(&sb, "+%s\n", bl[j])
	}
	return sb.String()
}
