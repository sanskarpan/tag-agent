package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
)

// PRD-076: high-value MCP server bundle.
//
// `mcp-registry list/install/enable/disable` could only ever act on ONE server
// at a time, so standing a profile up meant a dozen `enable` invocations and a
// dozen chances to miss one. `add-curated` is the bundle verb: it selects a
// slice of the embedded curated catalog (all of it, or one --category) and
// records + enables the whole slice in a single, atomic step.
//
// Install semantics follow the design decision already made for `plugin
// install` (see plugin.go): NATIVE record + enable, no npm/pip. TAG-Go has no
// package manager of its own and `mcp-registry install` shelling out to
// `npm install -g` is a host-wide side effect a bundle verb must not multiply.
// "Installed" here means what it means for plugins: recorded in the ledger
// (mcp_curated_installs) and enabled for the profile (its `config` block written
// into the profile's config.yaml mcp_servers), which is exactly what the runtime
// reads when it launches MCP servers.
//
// Honesty guard, also from the plugin.go precedent: a curated server may declare
// requires_env secrets (BRAVE_API_KEY, GITHUB_TOKEN, ...). A server whose secret
// is absent cannot work, so recording it would be a fake success. The default is
// therefore to REFUSE THE WHOLE BUNDLE, naming every server and the exact
// variable it is missing, and to write nothing at all — an all-or-nothing bundle
// is the only outcome a caller can reason about. `--skip-missing-env` is the
// explicit opt-in to install the satisfiable subset instead, and it still
// reports precisely what was left out and why.

// mcpCuratedTable is the self-ensured ledger of curated bundle installs. It
// mirrors plugins_installed (plugin.go) so both "native install" surfaces record
// state the same way, and it is self-ensuring for the same reason: the shared
// schema.sql is owned elsewhere.
const mcpCuratedTable = `CREATE TABLE IF NOT EXISTS mcp_curated_installs(
  profile TEXT, name TEXT, category TEXT, installed_at TEXT,
  PRIMARY KEY(profile, name))`

// curatedServer is one catalog entry resolved for planning.
type curatedServer struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	RequiresEnv []string `json:"requires_env"`
	Config      any      `json:"-"`
}

// curatedCatalog reads the embedded curated catalog into a name-sorted slice.
func curatedCatalog() ([]curatedServer, error) {
	servers, err := config.MCPRegistry()
	if err != nil {
		return nil, err
	}
	out := make([]curatedServer, 0, len(servers))
	for _, name := range sortedKeys(servers) {
		info := asMap(servers[name])
		s := curatedServer{
			Name:        name,
			Description: str(info["description"]),
			Category:    str(info["category"]),
			Config:      info["config"],
			RequiresEnv: []string{},
		}
		for _, e := range asSlice(info["requires_env"]) {
			if k := str(e); k != "" {
				s.RequiresEnv = append(s.RequiresEnv, k)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// curatedCategories returns the sorted set of categories present in the catalog,
// so an unknown --category can be rejected with the real answer rather than a
// hard-coded list that drifts from the YAML.
func curatedCategories(all []curatedServer) []string {
	seen := map[string]bool{}
	for _, s := range all {
		if s.Category != "" {
			seen[s.Category] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// selectCurated narrows the catalog to one category (empty = the whole bundle).
// An unrecognised category is a usage error (exit 2), never a silently empty
// bundle that would report "installed 0 servers" as success.
func selectCurated(all []curatedServer, category string) ([]curatedServer, error) {
	if category == "" {
		return all, nil
	}
	var out []curatedServer
	for _, s := range all {
		if s.Category == category {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, usageErrorf("unknown --category %q; the curated catalog has: %s",
			category, strings.Join(curatedCategories(all), ", "))
	}
	return out, nil
}

// missingEnvFor returns the requires_env keys of s that are set neither in this
// process's environment nor in the profile's .env — the same two sources
// `plugin install` consults, so the guard matches what the runtime will load.
func missingEnvFor(s curatedServer, penv map[string]bool) []string {
	var missing []string
	for _, key := range s.RequiresEnv {
		if _, present := os.LookupEnv(key); present && os.Getenv(key) != "" {
			continue
		}
		if penv[key] {
			continue
		}
		missing = append(missing, key)
	}
	return missing
}

// curatedPlanRow is one line of the install plan (and of --json output).
type curatedPlanRow struct {
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	MissingEnv []string `json:"missing_env,omitempty"`
	Action     string   `json:"action"` // install | already-enabled | skip-missing-env
}

// registerMCPCurated adds `mcp-registry add-curated` and `mcp-registry
// list-curated` to the command built in mcp_registry.go.
func registerMCPCurated(m *cobra.Command, app *App) {
	var lcCategory, lcProfile string
	listCurated := &cobra.Command{Use: "list-curated", Short: "Show the curated MCP bundle without installing", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, err := curatedCatalog()
			if err != nil {
				return err
			}
			// list-curated is a read-only browse verb, so an unknown category is
			// an empty result ([]), not an error: nothing was going to be written
			// either way and `--json` must still emit a valid empty array.
			var rows []curatedServer
			for _, s := range all {
				if lcCategory == "" || s.Category == lcCategory {
					rows = append(rows, s)
				}
			}
			enabled := map[string]bool{}
			if lcProfile != "" {
				if err := validProfileName(lcProfile); err != nil {
					return usageErrorf("%v", err)
				}
				_, pcfg, err := loadProfileConfig(app, lcProfile)
				if err != nil {
					return err
				}
				for k := range asMap(pcfg["mcp_servers"]) {
					enabled[k] = true
				}
			}
			if flagJSON {
				type jrow struct {
					curatedServer
					Installed bool `json:"installed"`
				}
				out := []jrow{} // [] not null on an empty selection
				for _, s := range rows {
					out = append(out, jrow{s, enabled[s.Name]})
				}
				return emitJSON(out)
			}
			if len(rows) == 0 {
				fmt.Println("No curated servers match.")
				return nil
			}
			fmt.Printf("%-30s %-14s %-10s %s\n", "Server", "Category", "Installed", "Needs")
			fmt.Println(strings.Repeat("-", 80))
			for _, s := range rows {
				mark := "no"
				if enabled[s.Name] {
					mark = "yes"
				}
				fmt.Printf("%-30s %-14s %-10s %s\n", s.Name, s.Category, mark, strings.Join(s.RequiresEnv, ", "))
			}
			return nil
		}}
	listCurated.Flags().StringVar(&lcCategory, "category", "", "only servers in this category")
	listCurated.Flags().StringVar(&lcProfile, "profile", "", "annotate with install status for this profile")

	var acCategory, acProfile string
	var acDryRun, acSkipMissing bool
	addCurated := &cobra.Command{Use: "add-curated", Short: "Record + enable a curated bundle of MCP servers for a profile", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, err := curatedCatalog()
			if err != nil {
				return err
			}
			selected, err := selectCurated(all, acCategory)
			if err != nil {
				return err
			}
			profile := strOr(acProfile, app.Cfg.MasterProfile())
			if err := validProfileName(profile); err != nil {
				return usageErrorf("%v", err)
			}
			pcfgPath, pcfg, err := loadProfileConfig(app, profile)
			if err != nil {
				return err
			}
			existing := asMap(pcfg["mcp_servers"])
			penv := profileEnvKeys(app, profile)

			var plan []curatedPlanRow
			var blocked []string
			for _, s := range selected {
				row := curatedPlanRow{Name: s.Name, Category: s.Category, Action: "install"}
				if _, ok := existing[s.Name]; ok {
					row.Action = "already-enabled"
					plan = append(plan, row)
					continue
				}
				if miss := missingEnvFor(s, penv); len(miss) > 0 {
					row.MissingEnv = miss
					row.Action = "skip-missing-env"
					blocked = append(blocked, fmt.Sprintf("%s needs %s", s.Name, strings.Join(miss, ", ")))
				}
				plan = append(plan, row)
			}

			// Honesty guard. Without --skip-missing-env a bundle with any
			// unsatisfiable server is refused whole: no partial write, and the
			// message names every server and every variable it is missing.
			if len(blocked) > 0 && !acSkipMissing {
				return fmt.Errorf("curated bundle not installed: %d server(s) declare environment variable(s) that are not set: %s; "+
					"set them (or pass --skip-missing-env to install only the satisfiable servers) — nothing was recorded",
					len(blocked), strings.Join(blocked, "; "))
			}

			if acDryRun {
				if flagJSON {
					return emitJSON(map[string]any{"profile": profile, "dry_run": true, "plan": plan})
				}
				fmt.Printf("Curated bundle plan for profile '%s' (%d servers, dry run — nothing written)\n", profile, len(plan))
				for _, r := range plan {
					note := ""
					if len(r.MissingEnv) > 0 {
						note = "  [missing: " + strings.Join(r.MissingEnv, ", ") + "]"
					}
					fmt.Printf("  %-30s %-14s %s%s\n", r.Name, r.Category, r.Action, note)
				}
				return nil
			}

			// Write phase. The profile config is written ONCE, after the whole
			// bundle has been assembled in memory, so a failure part-way leaves the
			// profile exactly as it was rather than half-enabled.
			byName := map[string]curatedServer{}
			for _, s := range selected {
				byName[s.Name] = s
			}
			if existing == nil {
				existing = map[string]any{}
			}
			installed := []string{}
			skipped := []curatedPlanRow{}
			already := []string{}
			for _, r := range plan {
				switch r.Action {
				case "already-enabled":
					already = append(already, r.Name)
				case "skip-missing-env":
					skipped = append(skipped, r)
				default:
					existing[r.Name] = byName[r.Name].Config
					installed = append(installed, r.Name)
				}
			}
			if len(installed) > 0 {
				pcfg["mcp_servers"] = existing
				if err := writeProfileConfig(pcfgPath, pcfg); err != nil {
					return err
				}
			}
			// Ledger AFTER the enable landed, so there is never a recorded-but-not-
			// enabled row (same ordering as plugin install).
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if _, err := db.Exec(mcpCuratedTable); err != nil {
				return err
			}
			now := time.Now().UTC().Format(time.RFC3339)
			for _, name := range installed {
				if _, err := db.Exec(`INSERT INTO mcp_curated_installs(profile,name,category,installed_at)
					VALUES(?,?,?,?) ON CONFLICT(profile,name) DO UPDATE SET
					  category=excluded.category, installed_at=excluded.installed_at`,
					profile, name, byName[name].Category, now); err != nil {
					return err
				}
			}
			if flagJSON {
				return emitJSON(map[string]any{
					"profile": profile, "installed": installed,
					"already_enabled": already, "skipped": skipped,
				})
			}
			fmt.Printf("Curated bundle installed for profile '%s': %d recorded + enabled, %d already enabled, %d skipped\n",
				profile, len(installed), len(already), len(skipped))
			for _, n := range installed {
				fmt.Printf("  + %s\n", n)
			}
			for _, r := range skipped {
				fmt.Printf("  - %s (NOT installed: missing %s)\n", r.Name, strings.Join(r.MissingEnv, ", "))
			}
			return nil
		}}
	addCurated.Flags().StringVar(&acCategory, "category", "", "install only servers in this category (default: the whole bundle)")
	addCurated.Flags().StringVar(&acProfile, "profile", "", "profile (default master)")
	addCurated.Flags().BoolVar(&acDryRun, "dry-run", false, "print the install plan and exit 0 without writing anything")
	addCurated.Flags().BoolVar(&acSkipMissing, "skip-missing-env", false,
		"install the satisfiable subset instead of refusing the bundle when a server's requires_env secret is absent")

	m.AddCommand(addCurated, listCurated)
}
