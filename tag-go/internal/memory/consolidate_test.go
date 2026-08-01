package memory

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestConsolidationDaemonExitsOnCancel is the SIGTERM contract: the CLI wraps
// the context with signal.NotifyContext, so "exits cleanly on SIGTERM" is
// exactly "returns nil promptly when the context is cancelled", even though the
// configured interval is far longer than the test's patience.
func TestConsolidationDaemonExitsOnCancel(t *testing.T) {
	db := memTestDB(t)
	Add(db, "p", "something worth keeping", "fact", 0.9)

	ctx, cancel := context.WithCancel(context.Background())
	cycles := 0
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- RunConsolidationDaemon(ctx, db, ConsolidationOptions{
			Interval: time.Hour, // deliberately long: cancel must not wait for it
			Profile:  "p",
			OnCycle: func(int, []GCResult, error) {
				mu.Lock()
				cycles++
				mu.Unlock()
			},
		})
	}()

	// Wait for the first cycle so we know the loop is inside its long wait.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := cycles
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("daemon never ran its first cycle")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled daemon must return nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop within 2s of cancellation — the wait is not interruptible")
	}
}

// TestConsolidationDaemonLeaksNoGoroutines: the loop must not spawn per-cycle
// goroutines, so goroutine count is flat across many cycles and returns to
// baseline after shutdown.
func TestConsolidationDaemonLeaksNoGoroutines(t *testing.T) {
	db := memTestDB(t)
	Add(db, "p", "keep me", "fact", 0.9)

	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.Gosched()
			time.Sleep(2 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}
	base := settle()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	seen := make(chan int, 64)
	go func() {
		done <- RunConsolidationDaemon(ctx, db, ConsolidationOptions{
			Interval: time.Millisecond,
			Profile:  "p",
			OnCycle: func(c int, _ []GCResult, _ error) {
				select {
				case seen <- c:
				default:
				}
			},
		})
	}()
	// Let it churn through many cycles.
	timeout := time.After(5 * time.Second)
	got := 0
	for got < 40 {
		select {
		case <-seen:
			got++
		case <-timeout:
			cancel()
			t.Fatalf("only %d cycles in 5s", got)
		}
	}
	during := runtime.NumGoroutine()
	if during > base+5 {
		t.Errorf("goroutines grew while running: base=%d during=%d", base, during)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
	after := settle()
	if after > base+2 {
		t.Errorf("goroutine leak after shutdown: base=%d after=%d", base, after)
	}
}

// TestConsolidationDaemonSurvivesCycleErrors: a background agent must not die
// on a transient failure; it reports and keeps going.
func TestConsolidationDaemonSurvivesCycleErrors(t *testing.T) {
	db := memTestDB(t)
	// Dropping the audit table makes RunGC fail on its final INSERT while leaving
	// semantic_memories intact — a realistic transient-ish failure.
	if _, err := db.Exec(`DROP TABLE memory_gc_runs`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 8)
	done := make(chan error, 1)
	go func() {
		done <- RunConsolidationDaemon(ctx, db, ConsolidationOptions{
			Interval: time.Millisecond,
			Profile:  "p",
			OnCycle: func(_ int, _ []GCResult, err error) {
				select {
				case errs <- err:
				default:
				}
			},
		})
	}()
	for i := 0; i < 3; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("expected the cycle to report an error")
			}
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("daemon stopped reporting after an error — it must keep running")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon must still exit cleanly, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

// TestConsolidationDaemonDoesNotStarveForegroundWrites: the store pins
// SetMaxOpenConns(1), so a cursor held across a cycle would deadlock every
// concurrent writer. Foreground writes must keep succeeding while the daemon
// churns.
func TestConsolidationDaemonDoesNotStarveForegroundWrites(t *testing.T) {
	db := memTestDB(t)
	for i := 0; i < 20; i++ {
		Add(db, "p", "seed memory number "+string(rune('a'+i)), "fact", 0.9)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunConsolidationDaemon(ctx, db, ConsolidationOptions{
			Interval: time.Millisecond, Profile: "p",
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	writes := 0
	for time.Now().Before(deadline) && writes < 50 {
		if _, err := Add(db, "p", "foreground write "+time.Now().Format(time.RFC3339Nano), "fact", 0.9); err != nil {
			cancel()
			t.Fatalf("foreground write starved by the daemon after %d writes: %v", writes, err)
		}
		writes++
	}
	if writes < 50 {
		cancel()
		t.Fatalf("only %d foreground writes completed in 3s — the daemon is holding the write lock", writes)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestConsolidationDaemonMaxCycles(t *testing.T) {
	db := memTestDB(t)
	Add(db, "p", "x", "fact", 0.9)
	n := 0
	err := RunConsolidationDaemon(context.Background(), db, ConsolidationOptions{
		Interval:  time.Millisecond,
		Profile:   "p",
		MaxCycles: 3,
		OnCycle:   func(int, []GCResult, error) { n++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("MaxCycles=3 ran %d cycles", n)
	}
}

// TestConsolidationDaemonSkipsWorkWhenPreCancelled: a daemon cancelled before it
// starts must not sneak in a surprise cycle.
func TestConsolidationDaemonSkipsWorkWhenPreCancelled(t *testing.T) {
	db := memTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	if err := RunConsolidationDaemon(ctx, db, ConsolidationOptions{
		Interval: time.Millisecond, Profile: "p",
		OnCycle: func(int, []GCResult, error) { n++ },
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pre-cancelled daemon ran %d cycles", n)
	}
}

func TestConsolidateOnceAllProfiles(t *testing.T) {
	db := memTestDB(t)
	Add(db, "a", "alpha memory", "fact", 0.9)
	Add(db, "b", "beta memory", "fact", 0.9)
	res, err := ConsolidateOnce(db, ConsolidationOptions{AllProfiles: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("expected one result per profile, got %+v", res)
	}
}

func TestValidateInterval(t *testing.T) {
	if err := ValidateInterval(0); err == nil {
		t.Error("zero interval must be rejected")
	}
	if err := ValidateInterval(-time.Second); err == nil {
		t.Error("negative interval must be rejected")
	}
	if err := ValidateInterval(time.Minute); err != nil {
		t.Errorf("valid interval rejected: %v", err)
	}
}
