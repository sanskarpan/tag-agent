package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tag-agent/tag/internal/store"
)

// personaIsBuiltin reports whether name is one of the bundled personas.
func personaIsBuiltin(name string) bool {
	for _, p := range builtinPersonas {
		if p.Name == name {
			return true
		}
	}
	return false
}

// personaBuildMergedPrompt mirrors persona.build_merged_prompt.
func personaBuildMergedPrompt(base string, personas []map[string]any) string {
	if len(personas) == 0 {
		return base
	}
	sorted := make([]map[string]any, len(personas))
	copy(sorted, personas)
	sort.SliceStable(sorted, func(i, j int) bool {
		pi, _ := sorted[i]["position"].(int)
		pj, _ := sorted[j]["position"].(int)
		return pi < pj
	})
	var prepend, append_ []string
	for _, p := range sorted {
		style := strings.TrimSpace(str(p["style_prompt"]))
		if str(p["inject"]) == "append" {
			append_ = append(append_, style)
		} else {
			prepend = append(prepend, style)
		}
	}
	var parts []string
	if len(prepend) > 0 {
		parts = append(parts, strings.Join(prepend, "\n\n"))
	}
	parts = append(parts, strings.TrimSpace(base))
	if len(append_) > 0 {
		parts = append(parts, strings.Join(append_, "\n\n"))
	}
	return strings.Join(parts, "\n\n")
}

// builtinPersona is a bundled persona (port of persona.py BUILTIN_PERSONAS).
type builtinPersona struct {
	Name, Description, Inject, StylePrompt string
	Tags                                   []string
}

var builtinPersonas = []builtinPersona{
	{"terse-engineer", "Terse, senior-engineer style: prefer code, skip preamble", "prepend",
		"You communicate as a terse senior software engineer. Skip preamble, avoid filler phrases, and prefer code samples over prose. Never say 'Certainly!', 'Of course!', or 'Great question!'. Be direct and precise.",
		[]string{"style", "engineering"}},
	{"verbose-explainer", "Detailed, tutorial-style explanations for learning contexts", "append",
		"Explain every concept in detail. Use analogies and examples. Break complex topics into numbered steps. Assume the reader is learning. Include 'why' explanations alongside 'how' steps.",
		[]string{"style", "education"}},
	{"security-focused", "Security-first lens: flag risks, OWASP references, secure defaults", "prepend",
		"Apply a security-first lens to every response. Flag OWASP Top 10 risks where relevant, recommend secure defaults, and always mention if a suggested approach has known CVEs or attack vectors. Prefer defense-in-depth recommendations.",
		[]string{"security", "domain"}},
	{"data-scientist", "Data science domain conventions: pandas, sklearn, Jupyter idioms", "append",
		"You work within a data science context. Use pandas, numpy, and scikit-learn idioms. Prefer vectorized operations over loops. Always mention data leakage risks in ML pipelines. Suggest visualization with matplotlib or seaborn where appropriate.",
		[]string{"domain", "data"}},
	{"teacher", "Socratic teaching style: ask guiding questions, scaffold understanding", "append",
		"Teach using the Socratic method. Ask guiding questions to help the user discover answers. Scaffold explanations from fundamentals upward. Provide worked examples before asking the user to try independently.",
		[]string{"style", "education"}},
}

// seedBuiltinPersonas idempotently inserts the bundled personas (INSERT OR
// IGNORE by unique name), mirroring Python's _seed_builtins called on every
// list/get. Without this the whole persona feature is empty.
func seedBuiltinPersonas(db *store.DB) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range builtinPersonas {
		tags, _ := json.Marshal(p.Tags)
		db.Exec(`INSERT OR IGNORE INTO personas(id,name,description,style_prompt,inject,tags_json,source,created_at)
			VALUES(?,?,?,?,?,?,'builtin',?)`, uuid.NewString()[:12], p.Name, p.Description, p.StylePrompt, p.Inject, string(tags), now)
	}
}

func registerPersona(root *cobra.Command, app *App) {
	var profile string
	p := &cobra.Command{Use: "persona", Short: "Agent persona management", GroupID: "tools"}
	p.PersistentFlags().StringVar(&profile, "profile", "", "profile")

	list := &cobra.Command{Use: "list", Short: "List personas",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			seedBuiltinPersonas(db)
			rows, err := db.Query(`SELECT name,description,source FROM personas ORDER BY name`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type persona struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Source      string `json:"source"`
			}
			var out []persona
			for rows.Next() {
				var pp persona
				if err := rows.Scan(&pp.Name, &pp.Description, &pp.Source); err != nil {
					return err
				}
				out = append(out, pp)
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Println("No personas.")
				return nil
			}
			for _, pp := range out {
				fmt.Printf("%-20s [%s] %s\n", pp.Name, pp.Source, pp.Description)
			}
			return nil
		}}
	var sessionID string
	apply := &cobra.Command{Use: "apply NAME", Short: "Apply a persona to a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, ok := app.Cfg.Profiles()[app.profile(profile)]; !ok {
				return fmt.Errorf("unknown profile '%s'", app.profile(profile))
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			seedBuiltinPersonas(db)
			var exists int
			db.QueryRow(`SELECT COUNT(*) FROM personas WHERE name=?`, args[0]).Scan(&exists)
			if exists == 0 {
				return fmt.Errorf("persona not found: %s", args[0])
			}
			var pos int
			db.QueryRow(`SELECT COALESCE(MAX(position),-1)+1 FROM active_personas WHERE profile=?`, app.profile(profile)).Scan(&pos)
			// session_id is NULL unless --session-id is given, matching Python's
			// apply_persona(session_id=None default); a stored value scopes the
			// stack so get_active_personas can filter by session.
			var sid any
			if sessionID != "" {
				sid = sessionID
			}
			// Upsert: re-applying an already-active persona updates its position
			// (and session) rather than inserting a duplicate row (PK is
			// profile+persona_name).
			_, err = db.Exec(`INSERT INTO active_personas(profile,persona_name,position,session_id,created_at) VALUES(?,?,?,?,?)
				ON CONFLICT(profile,persona_name) DO UPDATE SET position=excluded.position, session_id=excluded.session_id`,
				app.profile(profile), args[0], pos, sid, time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			fmt.Printf("Applied persona '%s' to '%s' [%d]\n", args[0], app.profile(profile), pos)
			return nil
		}}
	apply.Flags().StringVar(&sessionID, "session-id", "", "scope the persona to a session id")
	stack := &cobra.Command{Use: "stack", Short: "Show applied persona stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT persona_name,position FROM active_personas WHERE profile=? ORDER BY position`, app.profile(profile))
			if err != nil {
				return err
			}
			defer rows.Close()
			type stackEntry struct {
				Name     string `json:"name"`
				Position int    `json:"position"`
			}
			entries := []stackEntry{}
			for rows.Next() {
				var nm string
				var pos int
				if err := rows.Scan(&nm, &pos); err != nil {
					return err
				}
				entries = append(entries, stackEntry{Name: nm, Position: pos})
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(map[string]any{"profile": app.profile(profile), "stack": entries})
			}
			for _, e := range entries {
				fmt.Printf("[%d] %s\n", e.Position, e.Name)
			}
			if len(entries) == 0 {
				fmt.Printf("No personas applied to '%s'.\n", app.profile(profile))
			}
			return nil
		}}
	remove := &cobra.Command{Use: "remove NAME", Short: "Remove an applied persona", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r, err := db.Exec(`DELETE FROM active_personas WHERE profile=? AND persona_name=?`, app.profile(profile), args[0])
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				return jsonErrorMaybe(fmt.Errorf("persona not applied: %s", args[0]))
			}
			fmt.Println("removed")
			return nil
		}}
	show := &cobra.Command{Use: "show NAME", Short: "Show a persona", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			seedBuiltinPersonas(db)
			var id, name, desc, style, inject, tagsJSON, source string
			err = db.QueryRow(`SELECT id,name,description,style_prompt,inject,COALESCE(tags_json,'[]'),source FROM personas WHERE name=?`, args[0]).
				Scan(&id, &name, &desc, &style, &inject, &tagsJSON, &source)
			if err != nil {
				return jsonErrorMaybe(fmt.Errorf("Persona not found: %q", args[0]))
			}
			var tags []string
			json.Unmarshal([]byte(tagsJSON), &tags)
			if tags == nil {
				tags = []string{}
			}
			if flagJSON {
				return emitJSON(map[string]any{"id": id, "name": name, "description": desc,
					"style_prompt": style, "inject": inject, "tags": tags, "source": source})
			}
			fmt.Printf("Name:        %s\n", name)
			fmt.Printf("Description: %s\n", desc)
			fmt.Printf("Inject:      %s\n", inject)
			fmt.Printf("Tags:        %s\n", strings.Join(tags, ", "))
			fmt.Printf("Source:      %s\n", source)
			fmt.Printf("\nStyle Prompt:\n%s\n", style)
			return nil
		}}

	del := &cobra.Command{Use: "delete NAME", Short: "Delete an installed persona", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			r, err := db.Exec(`DELETE FROM personas WHERE name=? AND source!='builtin'`, args[0])
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				return fmt.Errorf("No installed persona named '%s' to delete (built-in personas cannot be deleted).", args[0])
			}
			fmt.Printf("Installed persona '%s' deleted.\n", args[0])
			return nil
		}}

	var basePrompt string
	preview := &cobra.Command{Use: "preview", Short: "Preview the merged prompt for a profile's persona stack", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			seedBuiltinPersonas(db)
			rows, err := db.Query(`SELECT persona_name,position FROM active_personas WHERE profile=? ORDER BY position`, app.profile(profile))
			if err != nil {
				return err
			}
			defer rows.Close()
			var actives []struct {
				name string
				pos  int
			}
			for rows.Next() {
				var nm string
				var pos int
				if err := rows.Scan(&nm, &pos); err != nil {
					return err
				}
				actives = append(actives, struct {
					name string
					pos  int
				}{nm, pos})
			}
			var personas []map[string]any
			for _, a := range actives {
				var style, inject string
				if err := db.QueryRow(`SELECT style_prompt,inject FROM personas WHERE name=?`, a.name).Scan(&style, &inject); err == nil {
					personas = append(personas, map[string]any{"style_prompt": style, "inject": inject, "position": a.pos})
				}
			}
			fmt.Println(personaBuildMergedPrompt(basePrompt, personas))
			return nil
		}}
	preview.Flags().StringVar(&basePrompt, "base-prompt", "You are a helpful agent.", "base system prompt")

	install := &cobra.Command{Use: "install FILE", Short: "Install a persona from a YAML file", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("Persona file not found: %s", args[0])
			}
			var raw map[string]any
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return fmt.Errorf("Invalid persona file: %v", err)
			}
			if raw == nil {
				return fmt.Errorf("Persona must be a YAML mapping")
			}
			if _, ok := raw["style_prompt"]; !ok {
				return fmt.Errorf("Persona must have a 'style_prompt' field")
			}
			name := str(raw["name"])
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0]))
				raw["name"] = name
			}
			if personaIsBuiltin(name) {
				return fmt.Errorf("'%s' is a built-in persona and cannot be overwritten; choose a different name.", name)
			}
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			var existingSource string
			if err := db.QueryRow(`SELECT source FROM personas WHERE name=?`, name).Scan(&existingSource); err == nil && existingSource == "builtin" {
				return fmt.Errorf("'%s' is a built-in persona and cannot be overwritten; choose a different name.", name)
			}
			inject := str(raw["inject"])
			if inject == "" {
				inject = "prepend"
			}
			var tags []string
			if ts, ok := raw["tags"].([]any); ok {
				for _, t := range ts {
					tags = append(tags, str(t))
				}
			}
			if tags == nil {
				tags = []string{}
			}
			tagsJSON, _ := json.Marshal(tags)
			pid := uuid.NewString()[:12]
			_, err = db.Exec(`INSERT INTO personas(id,name,description,style_prompt,inject,tags_json,source,created_at)
				VALUES(?,?,?,?,?,?,'user',?)
				ON CONFLICT(name) DO UPDATE SET description=excluded.description, style_prompt=excluded.style_prompt,
				inject=excluded.inject, tags_json=excluded.tags_json, source=excluded.source`,
				pid, name, str(raw["description"]), str(raw["style_prompt"]), inject, string(tagsJSON), time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			var finalID string
			db.QueryRow(`SELECT id FROM personas WHERE name=?`, name).Scan(&finalID)
			fmt.Printf("Persona '%s' installed (%s).\n", name, short(finalID))
			return nil
		}}

	p.AddCommand(list, apply, stack, remove, show, del, preview, install)
	root.AddCommand(p)
}
