package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/paths"
	"github.com/tag-agent/tag/internal/version"
)

func registerSystem(root *cobra.Command, app *App) {
	// doctor
	doctor := &cobra.Command{
		Use: "doctor", Short: "Validate local TAG paths and toolchain", GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Exported fields so `doctor --json` actually serializes them
			// (encoding/json skips unexported fields).
			type check struct {
				Name   string `json:"name"`
				Msg    string `json:"msg"`
				Status string `json:"status"`
			}
			checks := []check{
				{"tag_home", paths.Home(), "ok"},
				{"config", app.ConfigPath, "ok"},
				{"go_runtime", runtime.Version(), "ok"},
			}
			if _, err := os.Stat(app.ConfigPath); err != nil {
				checks[1].Status, checks[1].Msg = "fail", "missing"
			}
			git := "not found"
			if p, err := exec.LookPath("git"); err == nil {
				git = p
			}
			checks = append(checks, check{"git", git, ternary(git == "not found", "warn", "ok")})
			hasFail := false
			for _, c := range checks {
				if c.Status == "fail" {
					hasFail = true
				}
			}
			if flagJSON {
				out := map[string]any{"checks": checks, "ok": !hasFail}
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
			} else {
				fmt.Println("\nSYSTEM")
				for _, c := range checks {
					icon := map[string]string{"ok": "✓", "warn": "⚠", "fail": "✗"}[c.Status]
					fmt.Printf("  %s %-16s %s\n", icon, c.Name, c.Msg)
				}
			}
			if hasFail {
				return fmt.Errorf("doctor found failures")
			}
			return nil
		},
	}

	env := &cobra.Command{
		Use: "env", Short: "Print isolated environment values", GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt := app.Cfg.Section("runtime")
			home, _ := rt["home_dir"].(string)
			fmt.Printf("TAG_HOME=%s\n", paths.Home())
			fmt.Printf("HOME=%s\n", paths.RuntimeHome(home))
			return nil
		},
	}

	bootstrap := &cobra.Command{
		Use: "bootstrap", Short: "Create profile homes and render config", GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt := app.Cfg.Section("runtime")
			home, _ := rt["home_dir"].(string)
			db, _ := rt["db_path"].(string)
			if err := paths.EnsureRuntimeDirs(home, db); err != nil {
				return err
			}
			// #537c: report per-profile status so a repeat `bootstrap` shows
			// "exists" for already-present profile homes instead of "created".
			type profileStatus struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			statuses := []profileStatus{}
			for name := range app.Cfg.Profiles() {
				// #537e: profile names become filesystem path segments here, so
				// reject traversal / separator names before joining.
				if err := validProfileName(name); err != nil {
					return err
				}
				ph := paths.ProfileHome(home, name)
				existed := false
				if _, err := os.Stat(ph); err == nil {
					existed = true
				}
				if err := os.MkdirAll(ph, 0o755); err != nil {
					return err
				}
				status := "created"
				if existed {
					status = "exists"
				}
				statuses = append(statuses, profileStatus{name, status})
			}
			if flagJSON {
				b, _ := json.Marshal(map[string]any{"profiles": statuses})
				fmt.Println(string(b))
			} else {
				fmt.Println("Profiles:")
				for _, s := range statuses {
					fmt.Printf("  %s: %s\n", s.Name, s.Status)
				}
			}
			return nil
		},
	}

	setupCmd := &cobra.Command{
		Use: "setup", Short: "Provision the managed runtime", GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("native Go runtime — no external provisioning required (own-the-runtime end state)")
			return nil
		},
	}

	ver := &cobra.Command{
		Use: "version", Short: "Print version", GroupID: "system", Annotations: map[string]string{"noconfig": "1"},
		Run: func(cmd *cobra.Command, args []string) { fmt.Printf("tag %s\n", version.Version) },
	}

	root.AddCommand(doctor, env, bootstrap, setupCmd, ver)
}

func ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}
