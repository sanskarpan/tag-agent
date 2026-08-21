package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/guardrail"
)

// guardrailRuntimeEditCommands builds `guardrail runtime add` and `remove`.
//
// These edit the `tripwire:` block of tag.yaml. They deliberately do NOT mutate
// a running ruleset: PRD-123 NFR-03 makes the loaded registry immutable at
// process start, so an add/remove takes effect on the NEXT run, and every
// command says so rather than implying a live change. Every rule is validated
// through the same guardrail.ParseLayer the loader uses BEFORE the file is
// written, so a bad regex, an unknown type/action, or a duplicate name is
// refused at the CLI boundary instead of being written and breaking startup.
func guardrailRuntimeEditCommands(app *App) []*cobra.Command {
	var (
		profile, name, tool, typ, pattern, builtin, stage, action, message, window string
		threshold                                                                  int
	)
	add := &cobra.Command{
		Use:   "add",
		Short: "Add a guardrail rule to tag.yaml (effective NEXT run — the ruleset is immutable at process start)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" {
				return jsonErrorMaybe(usageErrorf("--name is required (it identifies the rule in findings and counters)"))
			}
			rule := map[string]any{"name": name}
			setIf(rule, "tool", tool)
			setIf(rule, "type", typ)
			setIf(rule, "stage", stage)
			setIf(rule, "pattern", pattern)
			setIf(rule, "builtin", builtin)
			setIf(rule, "action", action)
			setIf(rule, "message", message)
			setIf(rule, "window", window)
			if threshold > 0 {
				rule["threshold"] = threshold
			}

			// Resolve the target scope ONCE so the duplicate check reads exactly the
			// block the write will touch (top-level unless --profile is given).
			scoped := strings.TrimSpace(profile) != ""
			prof := ""
			if scoped {
				prof = app.profile(profile)
				// A typo'd profile must be refused, not silently written into a
				// phantom profiles.<typo> block that no run ever reads.
				if err := ensureProfileExists(app.Cfg, prof); err != nil {
					return jsonErrorMaybe(usageErr{err})
				}
			}

			path, err := config.Path(flagConfig)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			cur, err := config.Load(path)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			existing := tripwireRuleMaps(cur.Data, prof)
			for _, e := range existing {
				if m, ok := e.(map[string]any); ok {
					if n, _ := m["name"].(string); n == name {
						return jsonErrorMaybe(usageErrorf("a guardrail rule named %q already exists; remove it first or choose another name", name))
					}
				}
			}
			// Validate the WHOLE resulting block through the real loader, so any
			// error is caught here rather than written and breaking the next start.
			proposed := append(append([]any{}, existing...), rule)
			if _, err := guardrail.ParseLayer(map[string]any{"rules": proposed}, "validate"); err != nil {
				return jsonErrorMaybe(usageErr{err})
			}

			if _, err := config.Update(path, func(data map[string]any) {
				block := ensureTripwireBlock(data, prof, scoped)
				rules, _ := block["rules"].([]any)
				block["rules"] = append(rules, rule)
			}); err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(map[string]any{"added": name, "effective": "next run"})
			}
			fmt.Printf("added guardrail rule %q — effective on the NEXT run (the ruleset is loaded once at process start, NFR-03)\n", name)
			return nil
		},
	}
	add.Flags().StringVar(&profile, "profile", "", "profile whose tripwire block to edit (default: top-level)")
	add.Flags().StringVar(&name, "name", "", "rule name (required, unique)")
	add.Flags().StringVar(&tool, "tool", "", "tool-name glob the rule applies to (default *)")
	add.Flags().StringVar(&typ, "type", "", "rule type: pattern | tripwire | require-approval (default pattern)")
	add.Flags().StringVar(&pattern, "pattern", "", "regex matched against content (pattern rules)")
	add.Flags().StringVar(&builtin, "builtin", "", "built-in detector: secrets | destructive (pattern rules)")
	add.Flags().StringVar(&stage, "stage", "", fmt.Sprintf("screening stage %v (default: every stage)", guardrail.Stages()))
	add.Flags().StringVar(&action, "action", "", "action on match: block | warn | interrupt (default block; require-approval defaults to interrupt)")
	add.Flags().IntVar(&threshold, "threshold", 0, "tripwire threshold (tripwire rules)")
	add.Flags().StringVar(&window, "window", "", "tripwire window duration, e.g. 1h (tripwire rules)")
	add.Flags().StringVar(&message, "message", "", "message shown when the rule fires")

	var remProfile, remName string
	remove := &cobra.Command{
		Use:   "remove",
		Short: "Remove a guardrail rule from tag.yaml by name (effective NEXT run)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(remName) == "" {
				return jsonErrorMaybe(usageErrorf("--name is required"))
			}
			scoped := strings.TrimSpace(remProfile) != ""
			prof := ""
			if scoped {
				prof = app.profile(remProfile)
				if err := ensureProfileExists(app.Cfg, prof); err != nil {
					return jsonErrorMaybe(usageErr{err})
				}
			}
			path, err := config.Path(flagConfig)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			removed := false
			if _, err := config.Update(path, func(data map[string]any) {
				// Read-only lookup: a miss must not fabricate an empty tripwire block.
				block := lookupTripwireBlock(data, prof)
				if block == nil {
					return
				}
				rules, _ := block["rules"].([]any)
				kept := make([]any, 0, len(rules))
				for _, r := range rules {
					if m, ok := r.(map[string]any); ok {
						if n, _ := m["name"].(string); n == remName {
							removed = true
							continue
						}
					}
					kept = append(kept, r)
				}
				block["rules"] = kept
			}); err != nil {
				return jsonErrorMaybe(err)
			}
			if !removed {
				return jsonErrorMaybe(usageErrorf("no guardrail rule named %q found", remName))
			}
			if flagJSON {
				return emitJSON(map[string]any{"removed": remName, "effective": "next run"})
			}
			fmt.Printf("removed guardrail rule %q — effective on the NEXT run (NFR-03)\n", remName)
			return nil
		},
	}
	remove.Flags().StringVar(&remProfile, "profile", "", "profile whose tripwire block to edit (default: top-level)")
	remove.Flags().StringVar(&remName, "name", "", "name of the rule to remove (required)")

	return []*cobra.Command{add, remove}
}

// setIf writes a trimmed non-empty value, so an unset flag leaves the key absent
// and the loader applies its own default rather than seeing an empty string.
func setIf(m map[string]any, key, val string) {
	if strings.TrimSpace(val) != "" {
		m[key] = val
	}
}

// tripwireRuleMaps returns the raw rule maps from the tripwire block that
// applies (profile-scoped if prof is non-empty and present, else top-level).
func tripwireRuleMaps(data map[string]any, prof string) []any {
	block := lookupTripwireBlock(data, prof)
	if block == nil {
		return nil
	}
	rules, _ := block["rules"].([]any)
	return rules
}

// lookupTripwireBlock reads (never creates) the tripwire block for a profile,
// falling back to the top-level block, mirroring tripwireRules' resolution.
func lookupTripwireBlock(data map[string]any, prof string) map[string]any {
	if prof != "" {
		if profs, ok := data["profiles"].(map[string]any); ok {
			if pm, ok := profs[prof].(map[string]any); ok {
				if cfgm, ok := pm["config"].(map[string]any); ok {
					if b, ok := cfgm["tripwire"].(map[string]any); ok {
						return b
					}
				}
			}
		}
		return nil
	}
	if b, ok := data["tripwire"].(map[string]any); ok {
		return b
	}
	return nil
}

// ensureTripwireBlock returns the tripwire block, creating the nested maps when
// profileScoped is set so `add` can write into a profile that has none yet.
func ensureTripwireBlock(data map[string]any, prof string, profileScoped bool) map[string]any {
	if profileScoped {
		profs := childMap(data, "profiles")
		pm := childMap(profs, prof)
		cfgm := childMap(pm, "config")
		return childMap(cfgm, "tripwire")
	}
	return childMap(data, "tripwire")
}
