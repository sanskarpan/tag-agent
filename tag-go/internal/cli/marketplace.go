package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tag-agent/tag/internal/marketplace"
	"github.com/tag-agent/tag/internal/paths"
)

// mktEnsureTable self-ensures the marketplace_profiles cache table.
const mktEnsureTable = `CREATE TABLE IF NOT EXISTS marketplace_profiles(
  name TEXT PRIMARY KEY, source_url TEXT, sha256 TEXT, downloaded_at TEXT)`

// mktProfileConfigPath returns the runtime path the TAG runtime actually reads a
// profile from: runtime_home/.hermes/profiles/<name>/config.yaml. Mirrors
// Python's profile_home(cfg, name) / "config.yaml" (see cmd_profile_marketplace).
func mktProfileConfigPath(app *App, name string) string {
	homeDir := app.Cfg.String("runtime.home_dir", "")
	return filepath.Join(paths.ProfileHome(homeDir, name), "config.yaml")
}

// mktProfileName derives a profile name from an explicit --name or the URL stem.
func mktProfileName(explicit, rawURL string) string {
	if explicit != "" {
		return explicit
	}
	base := rawURL
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	base = filepath.Base(base)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

func registerMarketplace(root *cobra.Command, app *App) {
	mkt := &cobra.Command{
		Use:     "marketplace",
		Short:   "Profile marketplace: pull/push/list profiles",
		GroupID: "tools",
	}

	// ---- list ----
	mktList := &cobra.Command{
		Use:   "list",
		Short: "List cached marketplace profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if _, err := db.Exec(mktEnsureTable); err != nil {
				return err
			}
			rows, err := db.Query(`SELECT name, source_url, sha256, downloaded_at
				FROM marketplace_profiles ORDER BY name`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type mktRow struct {
				Name         string `json:"name"`
				SourceURL    string `json:"source_url"`
				SHA256       string `json:"sha256"`
				DownloadedAt string `json:"downloaded_at"`
			}
			var out []mktRow
			for rows.Next() {
				var r mktRow
				if err := rows.Scan(&r.Name, &r.SourceURL, &r.SHA256, &r.DownloadedAt); err != nil {
					return err
				}
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No cached profiles. Use `tag marketplace pull <url>` to add one.")
				return nil
			}
			for _, r := range out {
				when := r.DownloadedAt
				if len(when) > 10 {
					when = when[:10]
				}
				src := r.SourceURL
				if len(src) > 60 {
					src = src[:60]
				}
				fmt.Printf("  %-24s %-10s  %s\n", r.Name, when, src)
			}
			return nil
		},
	}

	// ---- pull ----
	var mktPullName string
	mktPull := &cobra.Command{
		Use:   "pull <url>",
		Short: "Download a profile from a URL (SSRF-guarded)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			name := mktProfileName(mktPullName, url)
			if name == "" {
				return fmt.Errorf("could not derive a profile name; pass --name")
			}
			// The name becomes a filename — reject traversal/separators.
			if err := validProfileName(name); err != nil {
				return err
			}
			if err := marketplace.ValidateFetchURL(url); err != nil {
				return fmt.Errorf("refused to fetch profile: %w", err)
			}
			content, err := marketplace.Fetch(url, 15*time.Second)
			if err != nil {
				return fmt.Errorf("failed to fetch profile: %w", err)
			}
			// Basic YAML validation: the runtime expects a mapping (mirrors Python).
			var pd map[string]any
			if err := yaml.Unmarshal(content, &pd); err != nil || pd == nil {
				return fmt.Errorf("invalid profile YAML: not a mapping")
			}
			sha := marketplace.SHA256Hex(content)

			// Write where the runtime actually reads profiles from, so the pulled
			// profile is immediately usable (not an inert file under managed/).
			localPath := mktProfileConfigPath(app, name)
			if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(localPath, content, 0o644); err != nil {
				return err
			}

			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if _, err := db.Exec(mktEnsureTable); err != nil {
				return err
			}
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := db.Exec(`INSERT INTO marketplace_profiles(name, source_url, sha256, downloaded_at)
				VALUES(?,?,?,?)
				ON CONFLICT(name) DO UPDATE SET
				  source_url=excluded.source_url, sha256=excluded.sha256,
				  downloaded_at=excluded.downloaded_at`,
				name, url, sha, now); err != nil {
				return err
			}

			if flagJSON {
				return emitJSON(map[string]any{"name": name, "sha256": sha, "local_path": localPath})
			}
			fmt.Printf("Pulled profile: %s\n", name)
			fmt.Printf("  SHA256: %s...\n", short16(sha))
			fmt.Printf("  Saved to: %s\n", localPath)
			return nil
		},
	}
	mktPull.Flags().StringVar(&mktPullName, "name", "", "local name for the profile (default: URL filename)")

	// ---- push ----
	var mktPushURL string
	mktPush := &cobra.Command{
		Use:   "push <name> --url <endpoint>",
		Short: "Push a profile's config to a marketplace endpoint (SSRF-guarded)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validProfileName(name); err != nil {
				return err
			}
			if mktPushURL == "" {
				return fmt.Errorf("--url <endpoint> is required")
			}

			// Read the profile's runtime config.yaml — the same file `pull` writes.
			pfile := mktProfileConfigPath(app, name)
			content, err := os.ReadFile(pfile)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("profile %q has no config at %s", name, pfile)
				}
				return err
			}
			// Validate it's a YAML mapping and re-encode as JSON for the wire.
			var pd map[string]any
			if err := yaml.Unmarshal(content, &pd); err != nil || pd == nil {
				return fmt.Errorf("invalid profile YAML: not a mapping")
			}
			body, err := json.Marshal(map[string]any{"name": name, "config": pd})
			if err != nil {
				return err
			}

			// Pre-flight the push URL with the SAME SSRF guard `pull` uses for
			// fetch; PushJSON also enforces it at the socket level + on redirects.
			if err := marketplace.ValidateFetchURL(mktPushURL); err != nil {
				return fmt.Errorf("refused to push profile: %w", err)
			}
			res, err := marketplace.PushJSON(mktPushURL, body, 15*time.Second)
			// A non-2xx returns both a non-nil error AND res (carrying the
			// server's status/body); only a transport-level failure has res==nil.
			// For machine consumers (--json), still emit the structured payload —
			// with status_code — on non-2xx, which is exactly the case they need
			// to parse; a non-nil return keeps the exit code non-zero.
			if flagJSON && res != nil {
				if e := emitJSON(map[string]any{
					"name":        name,
					"url":         mktPushURL,
					"status_code": res.StatusCode,
					"response":    string(res.Body),
				}); e != nil {
					return e
				}
				if err != nil {
					return fmt.Errorf("push failed: HTTP %d", res.StatusCode)
				}
				return nil
			}
			if err != nil {
				if res != nil {
					return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(res.Body)))
				}
				return fmt.Errorf("failed to push profile: %w", err)
			}
			fmt.Printf("Pushed profile: %s\n", name)
			fmt.Printf("  To: %s\n", mktPushURL)
			fmt.Printf("  Server: %s\n", res.Status)
			if len(res.Body) > 0 {
				fmt.Printf("  Response: %s\n", strings.TrimSpace(string(res.Body)))
			}
			return nil
		},
	}
	mktPush.Flags().StringVar(&mktPushURL, "url", "", "marketplace endpoint to POST the profile to (required)")

	mkt.AddCommand(mktList, mktPull, mktPush)
	root.AddCommand(mkt)
}

// short16 returns the first 16 chars of s (sha preview).
func short16(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
