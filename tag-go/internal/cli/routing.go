package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/config"
)

// registerRouting wires the config-driven routing commands: route, assignments,
// set-model, and models. Ports src/tag/cmd/routing.py + core/profile.py
// (resolve_route / collect_assignments / set-model). These are pure control-plane
// operations over the config profiles — no runtime/model calls needed.
func registerRouting(root *cobra.Command, app *App) {
	route := &cobra.Command{
		Use:     "route <task-type>",
		Short:   "Resolve the master/worker/verifier route for a task type",
		GroupID: "routing",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			masterOverride, _ := cmd.Flags().GetString("master-profile")
			workerOverride, _ := cmd.Flags().GetStringArray("worker-profile")
			masterModel, _ := cmd.Flags().GetString("master-model")
			verifierModel, _ := cmd.Flags().GetString("verifier-model")
			workerModels, _ := cmd.Flags().GetStringArray("worker-model")

			r, err := resolveRoute(app.Cfg, args[0], masterOverride, workerOverride)
			if err != nil {
				return jsonErrorMaybe(err)
			}
			if err := applyRouteModelOverrides(r, masterModel, verifierModel, workerModels); err != nil {
				return jsonErrorMaybe(err)
			}
			if flagJSON {
				return emitJSON(r)
			}
			fmt.Printf("task_type: %s\n", args[0])
			fmt.Printf("board: %s\n", r["board"])
			fmt.Printf("execution: %s\n", r["execution"])
			master := r["master"].(map[string]any)
			fmt.Printf("master: %s -> %s\n", master["name"], formatModelRef(asMap(master["model"])))
			for _, w := range r["workers"].([]map[string]any) {
				fmt.Printf("worker: %s -> %s\n", w["name"], formatModelRef(asMap(w["model"])))
			}
			if v, ok := r["verifier"].(map[string]any); ok && v != nil {
				fmt.Printf("verifier: %s -> %s\n", v["name"], formatModelRef(asMap(v["model"])))
			}
			return nil
		},
	}
	route.Flags().String("master-profile", "", "override master profile")
	route.Flags().StringArray("worker-profile", nil, "override worker profile(s)")
	route.Flags().String("master-model", "", "override master model (provider/model)")
	route.Flags().String("verifier-model", "", "override verifier model (provider/model)")
	route.Flags().StringArray("worker-model", nil, "override worker model (profile=provider/model)")

	assignments := &cobra.Command{
		Use:     "assignments",
		Short:   "List per-profile model assignments",
		GroupID: "routing",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rows := collectAssignments(app.Cfg)
			if flagJSON {
				return emitJSON(rows)
			}
			for _, row := range rows {
				runtime := ""
				if row["openai_runtime"] != "" {
					runtime = fmt.Sprintf(" [%s]", row["openai_runtime"])
				}
				fmt.Printf("%s: %s%s\n", row["profile"], row["primary_model"], runtime)
				if row["delegation_model"] != "-" {
					fmt.Printf("  delegation: %s\n", row["delegation_model"])
				}
			}
			return nil
		},
	}

	setModel := &cobra.Command{
		Use:     "set-model <profile> <provider/model>",
		Aliases: []string{"model"},
		Short:   "Set the primary or delegation model for a profile",
		GroupID: "routing",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, ref := args[0], args[1]
			target, _ := cmd.Flags().GetString("target")
			openaiRuntime, _ := cmd.Flags().GetString("openai-runtime")
			if target != "primary" && target != "delegation" {
				return fmt.Errorf("invalid --target %q (use primary|delegation)", target)
			}
			if err := ensureProfileExists(app.Cfg, profile); err != nil {
				return err
			}
			provider, model, err := parseModelRef(ref)
			if err != nil {
				return err
			}
			_, err = config.Update(app.ConfigPath, func(data map[string]any) {
				profiles := childMap(data, "profiles")
				pcfg := childMap(childMap(profiles, profile), "config")
				if target == "primary" {
					m := childMap(pcfg, "model")
					m["provider"] = provider
					m["default"] = model
					if openaiRuntime != "" {
						m["openai_runtime"] = openaiRuntime
					}
				} else {
					d := childMap(pcfg, "delegation")
					d["provider"] = provider
					d["model"] = model
					if openaiRuntime != "" {
						d["openai_runtime"] = openaiRuntime
					}
				}
			})
			if err != nil {
				return err
			}
			if flagJSON {
				res := map[string]any{"profile": profile, "target": target, "ref": provider + "/" + model, "config": app.ConfigPath}
				if openaiRuntime != "" {
					res["openai_runtime"] = openaiRuntime
				}
				return emitJSON(res)
			}
			fmt.Printf("%s %s model -> %s/%s\n", profile, target, provider, model)
			return nil
		},
	}
	setModel.Flags().String("target", "primary", "primary|delegation")
	setModel.Flags().String("openai-runtime", "", "optional openai runtime hint")

	models := &cobra.Command{
		Use:     "models <profile>",
		Short:   "List config-declared model assignments and providers for a profile",
		GroupID: "routing",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := args[0]
			if err := ensureProfileExists(app.Cfg, profile); err != nil {
				return err
			}
			pcfg := asMap(asMap(app.Cfg.Profiles()[profile])["config"])
			model := asMap(pcfg["model"])
			// Providers declared in env_examples (config-derived inventory; a live
			// provider catalog requires the runtime, which is Track B).
			providers := []string{}
			envEx := app.Cfg.Section("env_examples")
			for k := range asMap(envEx["shared"]) {
				providers = append(providers, strings.TrimSuffix(strings.ToLower(k), "_api_key"))
			}
			sort.Strings(providers)
			res := map[string]any{
				"profile":          profile,
				"current_provider": str(model["provider"]),
				"current_model":    str(model["default"]),
				"providers":        providers,
			}
			if flagJSON {
				return emitJSON(res)
			}
			fmt.Printf("profile: %s\n", profile)
			cur := "-"
			if res["current_provider"] != "" && res["current_model"] != "" {
				cur = fmt.Sprintf("%s/%s", res["current_provider"], res["current_model"])
			}
			fmt.Printf("current: %s\n", cur)
			fmt.Println("declared providers:")
			for _, p := range providers {
				fmt.Printf("  - %s\n", p)
			}
			return nil
		},
	}

	root.AddCommand(route, assignments, setModel, models)
}

// ---- routing helpers (port of core/profile.py) ----

func resolveRoute(cfg *config.Config, taskType, masterOverride string, workerOverride []string) (map[string]any, error) {
	routing := asMap(cfg.Section("routing")["task_types"])
	route := asMap(routing[taskType])
	if len(route) == 0 {
		avail := sortedKeys(routing)
		return nil, fmt.Errorf("unknown task type %q. Available: %s", taskType, strings.Join(avail, ", "))
	}
	master := masterOverride
	if master == "" {
		master = cfg.MasterProfile()
	}
	var workers []string
	if len(workerOverride) > 0 {
		workers = workerOverride
	} else {
		for _, w := range asSlice(route["workers"]) {
			workers = append(workers, str(w))
		}
	}
	// de-dup preserving order
	seen := map[string]bool{}
	deduped := workers[:0]
	for _, w := range workers {
		if !seen[w] {
			seen[w] = true
			deduped = append(deduped, w)
		}
	}
	workers = deduped

	profiles := cfg.Profiles()
	if _, ok := profiles[master]; !ok {
		return nil, fmt.Errorf("master profile %q is not defined in config", master)
	}
	snapshot := map[string]any{
		"master_profile": master,
		"board":          strOr(cfg.String("defaults.board", ""), "default"),
		"execution":      strOr(str(route["execution"]), "kanban"),
		"workers":        []map[string]any{},
		"verifier":       nil,
	}
	workerRows := []map[string]any{}
	for _, w := range workers {
		pdata, ok := profiles[w]
		if !ok {
			return nil, fmt.Errorf("worker profile %q is not defined in config", w)
		}
		pm := asMap(pdata)
		workerRows = append(workerRows, map[string]any{
			"name":        w,
			"description": str(pm["description"]),
			"tags":        pm["tags"],
			"model":       asMap(asMap(pm["config"])["model"]),
		})
	}
	snapshot["workers"] = workerRows

	if verifier := str(route["verifier"]); verifier != "" {
		vdata, ok := profiles[verifier]
		if !ok {
			return nil, fmt.Errorf("verifier profile %q is not defined in config", verifier)
		}
		vm := asMap(vdata)
		snapshot["verifier"] = map[string]any{
			"name":        verifier,
			"description": str(vm["description"]),
			"tags":        vm["tags"],
			"model":       asMap(asMap(vm["config"])["model"]),
		}
	}
	md := asMap(profiles[master])
	snapshot["master"] = map[string]any{
		"name":        master,
		"description": str(md["description"]),
		"tags":        md["tags"],
		"model":       asMap(asMap(md["config"])["model"]),
		"delegation":  asMap(asMap(md["config"])["delegation"]),
	}
	return snapshot, nil
}

func applyRouteModelOverrides(route map[string]any, masterModel, verifierModel string, workerModels []string) error {
	if masterModel != "" {
		p, m, err := parseModelRef(masterModel)
		if err != nil {
			return err
		}
		route["master"].(map[string]any)["model"] = map[string]any{"provider": p, "default": m}
	}
	if verifierModel != "" {
		if v, ok := route["verifier"].(map[string]any); ok && v != nil {
			p, m, err := parseModelRef(verifierModel)
			if err != nil {
				return err
			}
			v["model"] = map[string]any{"provider": p, "default": m}
		}
	}
	overrides := map[string][2]string{}
	for _, item := range workerModels {
		name, ref, ok := strings.Cut(item, "=")
		if !ok {
			return fmt.Errorf("invalid worker override %q. Use profile=provider/model-id", item)
		}
		p, m, err := parseModelRef(ref)
		if err != nil {
			return err
		}
		overrides[strings.TrimSpace(name)] = [2]string{p, m}
	}
	matched := map[string]bool{}
	for _, w := range route["workers"].([]map[string]any) {
		name := str(w["name"])
		if pm, ok := overrides[name]; ok {
			w["model"] = map[string]any{"provider": pm[0], "default": pm[1]}
			matched[name] = true
		}
	}
	var unknown []string
	for name := range overrides {
		if !matched[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("worker override names a non-worker profile: %s", strings.Join(unknown, ", "))
	}
	return nil
}

func collectAssignments(cfg *config.Config) []map[string]string {
	rows := []map[string]string{}
	names := sortedKeys(cfg.Profiles())
	for _, name := range names {
		profile := asMap(cfg.Profiles()[name])
		pcfg := asMap(profile["config"])
		primary := asMap(pcfg["model"])
		delegation := asMap(pcfg["delegation"])
		row := map[string]string{
			"profile":          name,
			"description":      str(profile["description"]),
			"primary_model":    formatModelRef(primary),
			"delegation_model": "-",
			"openai_runtime":   "",
		}
		if str(delegation["provider"]) != "" && str(delegation["model"]) != "" {
			row["delegation_model"] = str(delegation["provider"]) + "/" + str(delegation["model"])
		}
		if rt := str(primary["openai_runtime"]); rt != "" {
			row["openai_runtime"] = rt
		}
		rows = append(rows, row)
	}
	return rows
}

func parseModelRef(value string) (string, string, error) {
	for _, c := range value {
		if c < 32 || c == 127 {
			return "", "", fmt.Errorf("invalid model reference %q: control characters not allowed", value)
		}
	}
	ref := strings.TrimSpace(value)
	provider, model, ok := strings.Cut(ref, "/")
	provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	if !ok || provider == "" || model == "" {
		return "", "", fmt.Errorf("invalid model reference %q. Use provider/model-id format", value)
	}
	return provider, model, nil
}

func formatModelRef(m map[string]any) string {
	provider := strings.TrimSpace(str(m["provider"]))
	model := strings.TrimSpace(str(m["default"]))
	if model == "" {
		model = strings.TrimSpace(str(m["name"]))
	}
	if provider != "" && model != "" {
		return provider + "/" + model
	}
	if model != "" {
		return model
	}
	return "-"
}

func ensureProfileExists(cfg *config.Config, name string) error {
	if _, ok := cfg.Profiles()[name]; !ok {
		return fmt.Errorf("unknown profile %q. Available: %s", name, strings.Join(sortedKeys(cfg.Profiles()), ", "))
	}
	return nil
}
