package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/guardrail"
)

// guardrailContentCommand builds `tag guardrail input …` (PRD-122) or
// `tag guardrail output …` (PRD-121): dedicated content-validation surfaces over
// the shared content-guardrail engine (GuardrailResult, PRD-124).
//
// Exit codes for `test`: 0 clean · 2 usage · 3 a guardrail fired.
func guardrailContentCommand(app *App, direction string) *cobra.Command {
	validTypes := map[string]bool{}
	var typeList, actionList string
	if direction == "input" {
		for _, t := range []string{"prompt-injection", "pii", "secret", "topic-filter", "length-limit", "custom"} {
			validTypes[t] = true
		}
		typeList = "prompt-injection|pii|secret|topic-filter|length-limit|custom"
		actionList = "block|sanitize|warn"
	} else {
		for _, t := range []string{"pii", "secret", "json-schema", "topic-filter", "profanity", "toxicity", "custom"} {
			validTypes[t] = true
		}
		typeList = "pii|secret|json-schema|topic-filter|profanity|toxicity|custom"
		actionList = "block|rewrite|warn"
	}
	validActions := map[string]bool{"block": true, "warn": true}
	if direction == "input" {
		validActions["sanitize"] = true
	} else {
		validActions["rewrite"] = true
	}

	short := "Input guardrails (PRD-122): validate/sanitize user input before the model"
	if direction == "output" {
		short = "Output guardrails (PRD-121): screen model output (PII/secret/schema/…)"
	}
	c := &cobra.Command{Use: direction, Short: short}

	// ---- add ----
	var profile, gtype, action, severity, topics, schema, words, model string
	var maxLength int
	var threshold float64
	add := &cobra.Command{Use: "add", Short: "Add a " + direction + " guardrail to a profile", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validTypes[gtype] {
				return jsonErrorMaybe(usageErrorf("--type must be one of %s", typeList))
			}
			if action == "" {
				action = "block"
			}
			if !validActions[action] {
				return jsonErrorMaybe(usageErrorf("--action must be one of %s for %s guardrails", actionList, direction))
			}
			config := map[string]any{}
			switch gtype {
			case "length-limit":
				config["max_length"] = maxLength
			case "topic-filter":
				if topics != "" {
					config["topics"] = splitCSV(topics)
				}
				if cmd.Flags().Changed("threshold") {
					config["threshold"] = threshold
				}
			case "json-schema":
				if schema == "" {
					return jsonErrorMaybe(usageErrorf("--schema PATH is required for a json-schema guardrail"))
				}
				b, err := os.ReadFile(schema)
				if err != nil {
					return jsonErrorMaybe(usageErrorf("cannot read --schema %s: %v", schema, err))
				}
				var sc map[string]any
				if err := json.Unmarshal(b, &sc); err != nil {
					return jsonErrorMaybe(usageErrorf("--schema %s is not valid JSON: %v", schema, err))
				}
				config["schema"] = sc
			case "profanity":
				if words != "" {
					config["words"] = splitCSV(words)
				}
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			prof := app.profile(profile)
			id, err := guardrail.AddContentConfig(db.DB, direction, prof, gtype, action, config, strOr(severity, "high"), model)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(map[string]any{"id": id, "profile": prof, "type": gtype, "action": action, "direction": direction})
			}
			fmt.Printf("added %s guardrail %q (%s) for profile %q — id %s\n", direction, gtype, action, prof, id)
			return nil
		}}
	add.Flags().StringVar(&profile, "profile", "", "target profile (default: master profile)")
	add.Flags().StringVar(&gtype, "type", "", "guardrail type: "+typeList)
	add.Flags().StringVar(&action, "action", "", "on match: "+actionList+" (default block)")
	add.Flags().StringVar(&severity, "severity", "high", "high|medium|low")
	add.Flags().IntVar(&maxLength, "max-length", 4096, "length-limit type: max characters")
	add.Flags().StringVar(&topics, "topics", "", "topic-filter type: comma-separated forbidden topics")
	add.Flags().Float64Var(&threshold, "threshold", 0, "topic-filter similarity threshold")
	add.Flags().StringVar(&schema, "schema", "", "json-schema type: path to a JSON Schema file")
	add.Flags().StringVar(&words, "words", "", "profanity type: comma-separated words (extends defaults)")
	if direction == "input" {
		add.Flags().StringVar(&model, "classifier-model", "", "optional LLM classifier model")
	} else {
		add.Flags().StringVar(&model, "remediation-model", "", "model for the rewrite action")
	}

	// ---- list ----
	var listProfile string
	list := &cobra.Command{Use: "list", Short: "List configured " + direction + " guardrails", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			prof := app.profile(listProfile)
			cfgs, err := guardrail.ListContentConfigs(db.DB, direction, prof)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(cfgs)
			}
			if len(cfgs) == 0 {
				fmt.Printf("No %s guardrails configured for profile %q.\n", direction, prof)
				return nil
			}
			fmt.Printf("%d %s guardrail(s) for %q:\n", len(cfgs), direction, prof)
			for _, c := range cfgs {
				extra := ""
				if len(c.Config) > 0 {
					b, _ := json.Marshal(c.Config)
					extra = "  " + string(b)
				}
				fmt.Printf("  %s  %-16s %-9s [%s]%s\n", c.ID, c.Type, c.Action, c.Severity, extra)
			}
			return nil
		}}
	list.Flags().StringVar(&listProfile, "profile", "", "profile (default: master profile)")

	// ---- remove ----
	remove := &cobra.Command{Use: "remove ID", Short: "Remove a " + direction + " guardrail by id", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			ok, err := guardrail.RemoveContentConfig(db.DB, direction, args[0])
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if !ok {
				return jsonErrorMaybe(usageErrorf("no %s guardrail with id %q", direction, args[0]))
			}
			if flagJSON {
				return emitJSON(map[string]any{"removed": args[0]})
			}
			fmt.Printf("removed %s guardrail %s\n", direction, args[0])
			return nil
		}}

	// ---- test ----
	var testProfile, text, file string
	var stdin, exitZero bool
	test := &cobra.Command{Use: "test", Short: "Dry-run the " + direction + " guardrail chain against a string", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			content, err := readContent(cmd.InOrStdin(), text, file, stdin)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			prof := app.profile(testProfile)
			cfgs, err := guardrail.ListContentConfigs(db.DB, direction, prof)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if len(cfgs) == 0 {
				return jsonErrorMaybe(usageErrorf("no %s guardrails are configured for profile %q, so nothing was "+
					"checked — add one with `tag guardrail %s add` rather than treating this as a pass", direction, prof, direction))
			}
			v, err := guardrail.RunContentChain(db.DB, direction, prof, content, "test", false)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				if err := emitJSON(v); err != nil {
					return err
				}
			} else {
				if v.FinalAction == guardrail.GActionPass {
					fmt.Printf("clean (%s): no guardrail matched\n", direction)
				} else {
					fmt.Printf("%s (%s)\n", strings.ToUpper(string(v.FinalAction)), direction)
				}
				for _, r := range v.Results {
					fmt.Printf("  - %-16s %-9s %s\n", r.Guardrail, r.Action, r.Reason)
					if r.Message != nil {
						fmt.Printf("      note: %s\n", *r.Message)
					}
				}
				if v.FinalAction == guardrail.GActionSanitize {
					fmt.Printf("  sanitized → %s\n", v.Text)
				}
			}
			if v.FinalAction != guardrail.GActionPass && !exitZero {
				return exitCodeErr{code: exitFindings}
			}
			return nil
		}}
	test.Flags().StringVar(&testProfile, "profile", "", "profile (default: master profile)")
	test.Flags().StringVar(&text, "input", "", "content to screen")
	test.Flags().StringVar(&file, "file", "", "read content from a file")
	test.Flags().BoolVar(&stdin, "stdin", false, "read content from stdin")
	test.Flags().BoolVar(&exitZero, "exit-zero", false, "exit 0 even when a guardrail fires")

	// ---- history ----
	var histLimit int
	history := &cobra.Command{Use: "history", Short: "Show recent " + direction + " guardrail decisions", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if err := guardrail.EnsureContentSchema(db.DB, direction); err != nil {
				return jsonErrorMaybe(err)
			}
			rows, err := db.Query(`SELECT created_at, action, rule, blocked, detail FROM guardrail_events
				WHERE direction=? ORDER BY id DESC LIMIT ?`, direction, histLimit)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			defer rows.Close()
			type ev struct {
				CreatedAt string `json:"created_at"`
				Action    string `json:"action"`
				Guardrail string `json:"guardrail"`
				Blocked   bool   `json:"blocked"`
				Detail    string `json:"detail"`
			}
			out := []ev{}
			for rows.Next() {
				var e ev
				var blocked int
				if err := rows.Scan(&e.CreatedAt, &e.Action, &e.Guardrail, &blocked, &e.Detail); err != nil {
					return jsonErrorMaybe(err)
				}
				e.Blocked = blocked == 1
				out = append(out, e)
			}
			if flagJSON {
				return emitJSON(out)
			}
			if len(out) == 0 {
				fmt.Printf("no %s guardrail events recorded yet\n", direction)
				return nil
			}
			for _, e := range out {
				verdict := strings.ToUpper(e.Action)
				if e.Blocked {
					verdict = "BLOCK"
				}
				fmt.Printf("%s  %-10s %-16s %s\n", e.CreatedAt, verdict, e.Guardrail, e.Detail)
			}
			return nil
		}}
	history.Flags().IntVar(&histLimit, "limit", 50, "max rows")

	c.AddCommand(add, list, remove, test, history)
	return c
}

// buildContentScreeners returns the input/output screening hooks for the agent
// loop (PRD-122 FR-09 pre-model, PRD-121 FR-08 post-model), or nil when the
// profile has no guardrails of that direction configured — so a run without
// guardrails installs no hook and pays nothing. Decisions are persisted to the
// shared guardrail_events audit log (persist=true).
func buildContentScreeners(app *App, profile, runID string) (input, output func(string) (bool, string, string)) {
	db, err := app.OpenDB()
	if err != nil {
		return nil, nil
	}
	if cfgs, _ := guardrail.ListContentConfigs(db.DB, "input", profile); len(cfgs) > 0 {
		input = func(text string) (bool, string, string) {
			v, err := guardrail.RunContentChain(db.DB, "input", profile, text, runID, true)
			if err != nil {
				return false, "", ""
			}
			switch v.FinalAction {
			case guardrail.GActionBlock, guardrail.GActionInterrupt:
				return true, "", "input guardrail blocked: " + firstContentReason(v)
			case guardrail.GActionSanitize:
				return false, v.Text, ""
			}
			return false, "", ""
		}
	}
	if cfgs, _ := guardrail.ListContentConfigs(db.DB, "output", profile); len(cfgs) > 0 {
		output = func(text string) (bool, string, string) {
			v, err := guardrail.RunContentChain(db.DB, "output", profile, text, runID, true)
			if err != nil {
				return false, "", ""
			}
			if v.FinalAction == guardrail.GActionBlock || v.FinalAction == guardrail.GActionInterrupt {
				return true, "", "output guardrail blocked: " + firstContentReason(v)
			}
			return false, "", ""
		}
	}
	return input, output
}

func firstContentReason(v guardrail.ContentVerdict) string {
	for _, r := range v.Results {
		if r.Fired() {
			return r.Reason
		}
	}
	return string(v.FinalAction)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
