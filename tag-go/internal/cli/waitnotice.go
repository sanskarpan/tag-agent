package cli

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// waitNoticeAfter is how long a provider call may be silent before we say we
// are still waiting. Short enough that a stall does not read as a hang, long
// enough that an ordinary call never prints anything.
const waitNoticeAfter = 8 * time.Second

// startWaitNotice prints a one-line "still waiting" notice to w if the work it
// brackets has not finished within waitNoticeAfter, and returns a stop function
// that must be called when the work completes.
//
// Why this exists: a provider that accepts the TCP connection and then never
// responds produces zero output until ResponseHeaderTimeout fires — 60s by
// default (internal/llm.DefaultResponseHeaderTimeout). The eventual failure is
// honest (`net/http: timeout awaiting response headers`, exit 1), so this is a
// UX fix, not a correctness one: without it a bounded wait is indistinguishable
// from the silent hang this project treats as a hard bar.
//
// Deliberately NOT a shorter timeout. 60s is a considered choice for slow
// models; shortening it to make the CLI feel responsive would break legitimate
// long first-token latencies.
//
// The notice goes to stderr so stdout stays a parseable document and --json
// consumers are unaffected. stop() is idempotent and safe to call from a defer.
func startWaitNotice(w io.Writer) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-done:
		case <-time.After(waitNoticeAfter):
			fmt.Fprintf(w, "note: still waiting for the provider's first response "+
				"(no reply after %s; the request fails on its own if headers never arrive)\n",
				waitNoticeAfter)
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
