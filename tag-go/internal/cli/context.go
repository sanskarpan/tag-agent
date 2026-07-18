package cli

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/contextwin"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/store"
)

// registerContext wires `tag context` — the context-window inspector plus the
// native compress/trim mutation paths.
//
// Python parity (src/tag/cmd/workflow_mgmt.py:cmd_context, PRD-018): the command
// has three subcommands — `show` (default), `compress` and `trim`. In Python
// these shell out to the hermes runtime (`hermes sessions optimize/trim`). The
// Go port owns its runtime (Track B): a "session" is a run recorded in the
// runs/steps tables (id-prefix resolved, like `runs show`), and:
//
//	show     -> offline window/budget inspector (runtime-independent math)
//	compress -> assemble the session's stored turns, run a summarization pass
//	            through the native agent loop, and persist a compressed record
//	trim     -> keep only the last N assembled items and persist the trimmed set
//
// Both compress and trim default to the offline `echo` provider (no keys, no
// network); `--provider openai|anthropic` selects a real adapter.
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

	var compressProfile, compressSession, compressProvider string
	compress := &cobra.Command{Use: "compress", Short: "Summarize and compress a session context", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if compressSession == "" {
				return fmt.Errorf("provide --session-id")
			}
			return contextCompress(app, compressProfile, compressSession, compressProvider)
		}}
	compress.Flags().StringVar(&compressProfile, "profile", "", "profile (default: master profile)")
	compress.Flags().StringVar(&compressSession, "session-id", "", "session to compress (required)")
	compress.Flags().StringVar(&compressProvider, "provider", "echo", "llm provider (echo = offline)")

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
			return contextTrim(app, trimProfile, trimSession, trimKeepLast)
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
	fmt.Println("\nNote: this reports the profile window budget (offline). Use `context")
	fmt.Println("compress`/`trim` with a --session-id to operate on a stored run's context.")
	return nil
}

// contextEnsureSchema self-ensures the context_compressions table used to
// persist compressed/trimmed session records. It is not in schema.sql (it is a
// Go-port Track-B artifact), so — like split_runs — every mutation command
// creates it on first use.
func contextEnsureSchema(db *store.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS context_compressions (
		  id             TEXT PRIMARY KEY,
		  session_id     TEXT NOT NULL,
		  profile        TEXT NOT NULL,
		  action         TEXT NOT NULL,
		  provider       TEXT NOT NULL DEFAULT 'echo',
		  items_before   INTEGER NOT NULL DEFAULT 0,
		  items_after    INTEGER NOT NULL DEFAULT 0,
		  tokens_before  INTEGER NOT NULL DEFAULT 0,
		  tokens_after   INTEGER NOT NULL DEFAULT 0,
		  summary        TEXT,
		  created_at     TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_cc_session ON context_compressions(session_id);
	`)
	return err
}

// assembleSession loads the stored context for a session. A "session" is a run
// resolved by id prefix (parity with `runs show`). The run's prompt is the user
// turn; each recorded step contributes its prompt (user/tool input) and output
// (assistant text). Profile-scoped memory-journal entries are appended as
// persistent context so compression sees the durable facts too.
func assembleSession(db *store.DB, profile, session string) (runID string, items []contextwin.Item, err error) {
	var prompt string
	err = db.QueryRow(`SELECT id, prompt FROM runs WHERE id LIKE ?||'%' ORDER BY created_at DESC LIMIT 1`, session).
		Scan(&runID, &prompt)
	if err == sql.ErrNoRows {
		return "", nil, fmt.Errorf("session not found: %q (no matching run)", session)
	}
	if err != nil {
		return "", nil, err
	}
	if prompt != "" {
		items = append(items, contextwin.Item{Role: "user", Text: prompt})
	}

	rows, err := db.Query(`SELECT role, prompt, output FROM steps WHERE run_id=? ORDER BY id`, runID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var role, sp, out string
		if e := rows.Scan(&role, &sp, &out); e != nil {
			return "", nil, e
		}
		if sp != "" {
			items = append(items, contextwin.Item{Role: strOr(role, "user"), Text: sp})
		}
		if out != "" {
			items = append(items, contextwin.Item{Role: "assistant", Text: out})
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}

	// Durable, profile-scoped memory-journal context (best-effort; the table may
	// not exist in a minimal bootstrap — ignore that error).
	mrows, merr := db.Query(`SELECT key, value FROM memory_journal WHERE profile=? ORDER BY created_at`, profile)
	if merr == nil {
		defer mrows.Close()
		for mrows.Next() {
			var k, v string
			if e := mrows.Scan(&k, &v); e == nil {
				items = append(items, contextwin.Item{Role: "memory", Text: k + ": " + v})
			}
		}
	}
	return runID, items, nil
}

// contextCompress assembles the session, runs a summarization pass through the
// native agent loop, and persists a compressed record.
func contextCompress(app *App, profileFlag, session, provider string) error {
	profile := app.profile(profileFlag)
	prov, ok := llm.Registry[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q (available: %v)", provider, providerNames())
	}
	db, err := app.OpenDB()
	if err != nil {
		return err
	}
	if err := contextEnsureSchema(db); err != nil {
		return err
	}
	runID, items, err := assembleSession(db, profile, session)
	if err != nil {
		return err
	}
	tokensBefore := contextwin.TotalTokens(items)

	loop := &agent.Loop{Provider: prov}
	res, err := loop.Run(context.Background(), contextwin.SummaryPrompt(contextwin.Transcript(items)),
		agent.Options{
			Model:  app.Cfg.String("profiles."+profile+".config.model.default", ""),
			System: "You are a context compressor. Produce a concise summary that preserves key facts and decisions.",
		})
	if err != nil {
		return err
	}
	summary := res.FinalText
	tokensAfter := contextwin.EstimateTokens(summary)

	id := uuid.NewString()[:16]
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO context_compressions
		(id,session_id,profile,action,provider,items_before,items_after,tokens_before,tokens_after,summary,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, runID, profile, "compress", provider, len(items), 1, tokensBefore, tokensAfter, summary, now); err != nil {
		return fmt.Errorf("recording compression: %w", err)
	}

	if flagJSON {
		return emitJSON(map[string]any{
			"id": id, "session_id": runID, "action": "compress", "provider": provider,
			"items_before": len(items), "tokens_before": tokensBefore, "tokens_after": tokensAfter,
			"summary": summary,
		})
	}
	fmt.Printf("Compressed session %s (%s)\n", runID, provider)
	fmt.Printf("  items:  %d -> 1 summary\n", len(items))
	fmt.Printf("  tokens: %d -> %d (est.)\n", tokensBefore, tokensAfter)
	fmt.Printf("  record: %s\n", id)
	fmt.Printf("\nSummary:\n%s\n", summary)
	return nil
}

// contextTrim assembles the session and keeps only the last N items, persisting
// the trimmed set as a record (the kept turns joined as the summary body).
func contextTrim(app *App, profileFlag, session string, keepLast int) error {
	profile := app.profile(profileFlag)
	db, err := app.OpenDB()
	if err != nil {
		return err
	}
	if err := contextEnsureSchema(db); err != nil {
		return err
	}
	runID, items, err := assembleSession(db, profile, session)
	if err != nil {
		return err
	}
	tokensBefore := contextwin.TotalTokens(items)
	kept := contextwin.KeepLast(items, keepLast)
	tokensAfter := contextwin.TotalTokens(kept)
	body := contextwin.Transcript(kept)

	id := uuid.NewString()[:16]
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO context_compressions
		(id,session_id,profile,action,provider,items_before,items_after,tokens_before,tokens_after,summary,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, runID, profile, "trim", "none", len(items), len(kept), tokensBefore, tokensAfter, body, now); err != nil {
		return fmt.Errorf("recording trim: %w", err)
	}

	if flagJSON {
		return emitJSON(map[string]any{
			"id": id, "session_id": runID, "action": "trim", "keep_last": keepLast,
			"items_before": len(items), "items_after": len(kept),
			"tokens_before": tokensBefore, "tokens_after": tokensAfter,
		})
	}
	fmt.Printf("Trimmed session %s to last %d turn(s)\n", runID, keepLast)
	fmt.Printf("  items:  %d -> %d\n", len(items), len(kept))
	fmt.Printf("  tokens: %d -> %d (est.)\n", tokensBefore, tokensAfter)
	fmt.Printf("  record: %s\n", id)
	return nil
}
