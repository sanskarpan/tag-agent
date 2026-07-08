package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/contextwin"
)

// registerContext wires `tag context` — the context-window inspector.
//
// Python parity (src/tag/cmd/workflow_mgmt.py:cmd_context, PRD-018): the
// command has three subcommands — `show` (default), `compress` and `trim` —
// and every one of them shells out to the hermes runtime binary:
//
//	show     -> hermes sessions list          (live per-session token counts)
//	compress -> hermes sessions optimize <id> (summarize + compress a session)
//	trim     -> hermes sessions optimize <id> (drop older turns)
//
// The Go port is offline and read-only, so the live runtime surfaces are not
// reachable here. `context show` therefore reports the profile's context
// *budget* (the window size and 0 live usage) using the runtime-independent
// math ported into internal/contextwin — this is the "window/budget inspector"
// view. `compress` and `trim` mutate live session state through the hermes
// runtime (Track-B), so they are honest stubs that clearly say so rather than
// silently no-op.
func registerContext(root *cobra.Command, app *App) {
	c := &cobra.Command{Use: "context", Short: "Inspect the agent context window budget", GroupID: "tools"}

	var showProfile string
	show := &cobra.Command{Use: "show", Short: "Show the context window budget for a profile", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return contextShow(app, showProfile)
		}}
	show.Flags().StringVar(&showProfile, "profile", "", "profile (default: master profile)")

	// Bare `tag context` mirrors Python's `sub is None -> show` default.
	c.RunE = func(cmd *cobra.Command, args []string) error {
		return contextShow(app, showProfile)
	}
	c.Flags().StringVar(&showProfile, "profile", "", "profile (default: master profile)")

	var compressProfile, compressSession string
	compress := &cobra.Command{Use: "compress", Short: "Summarize and compress a session context", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if compressSession == "" {
				return fmt.Errorf("provide --session-id")
			}
			return contextRuntimeStub("compress", compressSession)
		}}
	compress.Flags().StringVar(&compressProfile, "profile", "", "profile (default: master profile)")
	compress.Flags().StringVar(&compressSession, "session-id", "", "session to compress (required)")

	var trimProfile, trimSession string
	var trimKeepLast int
	trim := &cobra.Command{Use: "trim", Short: "Trim a session to the last N turns", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if trimSession == "" {
				return fmt.Errorf("provide --session-id")
			}
			if trimKeepLast <= 0 {
				return fmt.Errorf("--keep-last must be a positive integer")
			}
			return contextRuntimeStub("trim", trimSession)
		}}
	trim.Flags().StringVar(&trimProfile, "profile", "", "profile (default: master profile)")
	trim.Flags().StringVar(&trimSession, "session-id", "", "session to trim (required)")
	trim.Flags().IntVar(&trimKeepLast, "keep-last", 10, "number of most-recent turns to keep")

	c.AddCommand(show, compress, trim)
	root.AddCommand(c)
}

// contextShow reports the offline context-window budget for a profile. Live
// session usage requires the hermes runtime and is not reachable offline, so
// used_tokens is reported as 0 against the default window (mirrors the
// all-zeros failure shape of context.py:get_context_size, with max_tokens
// filled from DEFAULT_MAX_TOKENS).
func contextShow(app *App, profileFlag string) error {
	profile := app.profile(profileFlag)
	usage := contextwin.Usage{
		Profile:    profile,
		UsedTokens: 0,
		MaxTokens:  contextwin.DefaultMaxTokens,
		Pct:        contextwin.Pct(0, contextwin.DefaultMaxTokens),
	}
	if flagJSON {
		return emitJSON(usage)
	}
	fmt.Printf("Context window for profile '%s'\n", profile)
	fmt.Printf("  used:  %d tokens\n", usage.UsedTokens)
	fmt.Printf("  max:   %d tokens\n", usage.MaxTokens)
	fmt.Printf("  usage: %.2f%%\n", usage.Pct)
	fmt.Println("\nNote: live per-session usage requires the hermes runtime and is not")
	fmt.Println("available in the offline Go port; this reports the window budget only.")
	return nil
}

// contextRuntimeStub is the honest stub for the compress/trim mutation paths,
// which in Python drive `hermes sessions optimize`. There is no runtime to
// drive offline, so we say so plainly and exit non-zero rather than pretend.
func contextRuntimeStub(action, session string) error {
	if flagJSON {
		_ = emitJSON(map[string]any{
			"error":   fmt.Sprintf("context %s requires the hermes runtime (not available in the offline Go port)", action),
			"session": session,
		})
		return fmt.Errorf("context %s unavailable offline", action)
	}
	return fmt.Errorf("context %s requires the hermes runtime (Track-B) and is not available in the offline Go port", action)
}
