package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PRD-068 — background sleep-time memory consolidation.
//
// The on-demand pipeline (RunGC: evict → merge → promote) is unchanged; this
// file only adds the long-running driver around it, following the shape the
// cron daemon already proved out:
//
//   - the wait is a `select` on ctx.Done() and a timer, never time.Sleep, so a
//     SIGTERM lands within microseconds instead of at the end of the interval;
//   - each cycle opens and CLOSES every cursor before it mutates anything. The
//     store pins SetMaxOpenConns(1), so a cursor held across a write would
//     deadlock the single connection and starve every foreground command. The
//     existing GC phases already read-then-close; the daemon must not add a
//     long-lived query of its own, and it doesn't;
//   - no goroutine is spawned per cycle, so there is nothing to leak.
//
// The daemon performs no LLM calls and executes no tools, so there is no consent
// prompt it could ever block on.

// DefaultConsolidationInterval is the sleep-time cadence when none is given.
const DefaultConsolidationInterval = time.Hour

// ConsolidationOptions configures the background consolidation daemon.
type ConsolidationOptions struct {
	// Interval between cycles. Zero means DefaultConsolidationInterval.
	Interval time.Duration
	// Profile scopes the run; empty with AllProfiles=false uses Profile as-is.
	Profile string
	// AllProfiles consolidates every profile present in semantic_memories.
	AllProfiles bool
	// Cfg holds the GC thresholds (zero value → DefaultGCConfig).
	Cfg GCConfig
	// MaxCycles bounds the number of cycles (0 = unbounded). Tests use it; the
	// CLI leaves it at 0 and relies on the context.
	MaxCycles int
	// OnCycle is called after every cycle with its results. A cycle error is
	// reported here and the daemon keeps running: a transient DB error must not
	// silently kill a background agent.
	OnCycle func(cycle int, results []GCResult, err error)
}

// ConsolidateOnce runs one consolidation cycle and returns its per-profile
// results. Exposed separately so the daemon and `mem2 gc` share one code path.
func ConsolidateOnce(db *sql.DB, opts ConsolidationOptions) ([]GCResult, error) {
	cfg := opts.Cfg
	if cfg == (GCConfig{}) {
		cfg = DefaultGCConfig()
	}
	if opts.AllProfiles {
		return RunGCAllProfiles(db, cfg)
	}
	r, err := RunGC(db, opts.Profile, cfg)
	if err != nil {
		return nil, err
	}
	return []GCResult{r}, nil
}

// RunConsolidationDaemon blocks, running a consolidation cycle every Interval
// until ctx is cancelled (SIGINT/SIGTERM at the CLI layer) or MaxCycles is
// reached. It returns nil on a clean shutdown — a cancelled context is a normal
// exit, not a failure.
func RunConsolidationDaemon(ctx context.Context, db *sql.DB, opts ConsolidationOptions) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultConsolidationInterval
	}
	// One reusable timer instead of time.After per iteration: time.After leaks a
	// timer per loop until it fires, which for a long-lived daemon on a 1h
	// interval is a slow but real accumulation.
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for cycle := 1; ; cycle++ {
		// Check for cancellation BEFORE doing work, so a daemon cancelled during
		// startup never performs a surprise cycle.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		results, err := ConsolidateOnce(db, opts)
		if opts.OnCycle != nil {
			opts.OnCycle(cycle, results, err)
		}
		if opts.MaxCycles > 0 && cycle >= opts.MaxCycles {
			return nil
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}

// ValidateInterval rejects a non-positive interval with a message the CLI can
// classify as a usage error.
func ValidateInterval(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("--interval must be greater than zero, got %s", d)
	}
	return nil
}
