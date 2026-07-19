package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/version"
)

// jsonErrorMaybe, when --json is set, prints a parseable {"error": ...} object
// to stdout so a --json consumer gets structured output on the error path
// (issue #530). It still returns the error so the exit code stays non-zero.
func jsonErrorMaybe(err error) error {
	if err != nil && flagJSON {
		b, _ := json.Marshal(map[string]any{"error": err.Error()})
		fmt.Println(string(b))
	}
	return err
}

// parsePassed is set once cobra has resolved the target command, parsed its
// flags, and validated its args (the root PersistentPreRunE only runs after all
// of that succeeds). Usage errors — unknown command, bad flag, arg-count —
// happen before it is set.
var parsePassed bool

// isUsageError reports whether err is a cobra usage/argument error, which
// Python's argparse maps to exit code 2 (issue #531). Genuine runtime failures
// stay at exit 1.
func isUsageError(err error) bool {
	if err == nil {
		return false
	}
	// Explicitly-marked usage errors (bad flag VALUES rejected by a command's
	// own validation) are exit 2 regardless of message text (#537a).
	var ue usageErr
	if errors.As(err, &ue) {
		return true
	}
	return !parsePassed
}

var (
	flagConfig string
	flagJSON   bool
)

// NewRoot builds the tag root command with all groups attached.
func NewRoot() *cobra.Command {
	app := &App{}
	root := &cobra.Command{
		Use:           "tag",
		Short:         "TAG — the Agent Gateway CLI (native Go)",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			parsePassed = true
			// commands that don't need config opt out via annotation
			if cmd.Annotations["noconfig"] == "1" {
				return nil
			}
			return app.Load(flagConfig)
		},
	}
	root.PersistentFlags().StringVar(&flagConfig, "config", "", "path to tag.yaml")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "JSON output where supported")

	root.AddGroup(
		&cobra.Group{ID: "system", Title: "System:"},
		&cobra.Group{ID: "memory", Title: "Memory:"},
		&cobra.Group{ID: "routing", Title: "Routing:"},
		&cobra.Group{ID: "orch", Title: "Orchestration:"},
		&cobra.Group{ID: "tools", Title: "Agent tools:"},
		&cobra.Group{ID: "obs", Title: "Observability:"},
	)

	// register command groups (each file adds its commands)
	registerSystem(root, app)
	registerMemory(root, app)
	registerBudget(root, app)
	registerPersona(root, app)
	registerRouteFallback(root, app)
	registerRouting(root, app)
	registerCron(root, app)
	registerQueue(root, app)
	registerSecurity(root, app)
	registerWorkspace(root, app)
	registerObservability(root, app)
	registerNotify(root, app)
	registerGraph(root, app)
	registerPrompt(root, app)
	registerAlert(root, app)
	registerAnnotate(root, app)
	registerEvalDataset(root, app)
	registerMem2(root, app)
	registerDiffContext(root, app)
	registerHooks(root, app)
	registerMCPRegistry(root, app)
	registerTemplate(root, app)
	registerCompare(root, app)
	registerPlugin(root, app)
	registerEval(root, app)
	registerSwarm(root, app)
	registerRun(root, app)
	registerServe(root, app)
	registerGateway(root, app)
	registerToolIndex(root, app)
	registerCache(root, app)
	registerOtelExport(root, app)
	registerWebhook(root, app)
	registerImports(root, app)
	registerMCPServe(root, app)
	registerEvalCI(root, app)
	registerCI(root, app)
	registerMarketplace(root, app)
	registerAgentops(root, app)
	registerShell(root, app)
	registerMCPConnect(root, app)
	registerDevui(root, app)
	registerWeb(root, app)
	registerLSP(root, app)
	registerTUI(root, app)
	registerRuns(root, app)
	registerLogs(root, app)
	registerPromptSize(root, app)
	registerBenchmark(root, app)
	registerSandbox(root, app)
	registerEvalJudge(root, app)
	registerSWESolve(root, app)
	registerIssueSolve(root, app)
	registerAgenticCI(root, app)
	registerReviewPR(root, app)
	registerContext(root, app)
	registerSplit(root, app)

	// #562: cobra adds its `completion` command lazily at Execute() time, after
	// NewRoot() runs — so enforceUnknownSubcommand never walks it, and both a
	// missing shell (`tag completion`) and an unknown shell (`tag completion
	// badshell`) silently printed help and exited 0. Force the completion command
	// to materialize now, then constrain it to the known shells so a bad/missing
	// shell is a usage error (exit 2).
	root.InitDefaultCompletionCmd()
	enforceCompletionShell(root)

	// #535: give every pure group command (has subcommands, no Run/RunE of its
	// own) a RunE so an unknown SUBCOMMAND becomes a usage error (exit 2) while a
	// bare group still prints help (exit 0). Done generically so we never edit
	// each group's file.
	enforceUnknownSubcommand(root)
	return root
}

// enforceCompletionShell makes `tag completion` require exactly one of the known
// shell names as its argument. Without a valid shell (missing or unknown) it
// returns a usage error, which isUsageError() maps to exit 2 — instead of
// cobra's default of printing help and exiting 0 (#562).
func enforceCompletionShell(root *cobra.Command) {
	for _, c := range root.Commands() {
		if c.Name() != "completion" {
			continue
		}
		valid := make([]string, 0, len(c.Commands()))
		for _, sub := range c.Commands() {
			valid = append(valid, sub.Name())
		}
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErrorf("completion requires a shell argument, one of: %v", valid)
			}
			return usageErrorf("unknown shell %q for completion; expected one of: %v", args[0], valid)
		}
		return
	}
}

// enforceUnknownSubcommand walks the command tree and, for each command that
// dispatches to subcommands but has no Run/RunE of its own, installs a RunE that:
//   - prints help and exits 0 when invoked with no args (`tag mem`), preserving
//     the existing bare-group help behavior, and
//   - returns an "unknown command" usage error when invoked with a stray arg
//     (`tag mem bogussub`), which isUsageError() maps to exit 2.
//
// Cobra's default legacyArgs only rejects unknown args for the ROOT command (it
// short-circuits for any command with a parent), which is why unknown top-level
// commands already exit 2 but unknown subcommands silently printed help and
// exited 0. Runnable groups (e.g. `lsp`, which shows status) already have a
// RunE, so they are skipped and keep their behavior.
func enforceUnknownSubcommand(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		enforceUnknownSubcommand(c)
	}
	if cmd.HasSubCommands() && cmd.Run == nil && cmd.RunE == nil {
		cmd.RunE = func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return usageErrorf("unknown command %q for %q", args[0], c.CommandPath())
		}
	}
}

// Execute runs the root command.
func Execute() int {
	if err := NewRoot().Execute(); err != nil {
		// #537(d): translate raw SQLite open failures (e.g. a read-only TAG_HOME
		// yielding "unable to open database file (14)") into a friendly, actionable
		// message before they reach the user.
		err = friendlyDBError(err)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		if isUsageError(err) {
			return 2 // parity with Python argparse usage-error exit code (#531)
		}
		return 1
	}
	return 0
}
