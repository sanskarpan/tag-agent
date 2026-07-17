package llm

import (
	"context"
	"fmt"
	"strings"
)

// FallbackStep is one attempt in a provider fallback chain. Provider is the
// adapter to call; Model overrides Request.Model for this step (empty = keep the
// request's model). Condition gates whether an error from the PREVIOUS attempt
// is allowed to advance to this step (empty/"always"/"any" = any retryable
// error). The chain's first step (the primary) ignores Condition.
type FallbackStep struct {
	Provider  Provider
	Model     string
	Condition string
}

// FallbackProvider tries an ordered chain of providers, advancing to the next
// eligible step on a retryable error that occurs BEFORE any content streams. The
// first step that produces content (or completes cleanly) wins and streams
// through live — so the successful provider is not buffered. This is the runtime
// execution of the route_fallbacks config (gap #2): declared chains are actually
// walked on 429/401/400/timeout/overload during inference.
type FallbackProvider struct {
	Steps []FallbackStep
	// Retryable decides whether an error is transient enough to warrant falling
	// back at all. Defaults to DefaultRetryable. A step is taken only if the
	// error is Retryable AND matches the step's Condition.
	Retryable func(error) bool
	// OnFallback, if set, is called each time the chain advances past a failed
	// step (for logging/telemetry): the failed step index, the model it tried,
	// and the error that triggered the fallback.
	OnFallback func(stepIndex int, model string, err error)
}

// Name identifies the composite provider.
func (f *FallbackProvider) Name() string { return "fallback" }

// Stream walks the chain. On a retryable, condition-matched error before any
// content, it advances to the next eligible step; the winner streams live.
func (f *FallbackProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if len(f.Steps) == 0 {
		return nil, fmt.Errorf("fallback: no providers configured")
	}
	retry := f.Retryable
	if retry == nil {
		retry = DefaultRetryable
	}

	var lastErr error
	i := 0
	for i < len(f.Steps) {
		step := f.Steps[i]
		if step.Provider == nil {
			// A configured fallback whose provider slug isn't registered — skip it.
			lastErr = fmt.Errorf("fallback step %d has no registered provider", i)
			i = f.nextEligible(i, lastErr, retry)
			continue
		}
		r := req
		if step.Model != "" {
			r.Model = step.Model
		}
		ch, err := step.Provider.Stream(ctx, r)
		if err != nil {
			lastErr = err
			if next := f.nextEligible(i, err, retry); next > i {
				if f.OnFallback != nil {
					f.OnFallback(i, r.Model, err)
				}
				i = next
				continue
			}
			return nil, err
		}
		// Peek the first event so an early error (before content) can still fall
		// back; a provider that has already streamed text cannot be un-committed.
		first, ok := <-ch
		if !ok {
			// Empty completion — a clean (if empty) success.
			out := make(chan Event)
			close(out)
			return out, nil
		}
		if first.Type == EventError && first.Err != nil {
			lastErr = first.Err
			if next := f.nextEligible(i, first.Err, retry); next > i {
				if f.OnFallback != nil {
					f.OnFallback(i, r.Model, first.Err)
				}
				go drainEvents(ch)
				i = next
				continue
			}
			// Last eligible / non-retryable — forward the error stream unchanged.
			out := make(chan Event, 1)
			out <- first
			go func() {
				for ev := range ch {
					out <- ev
				}
				close(out)
			}()
			return out, nil
		}
		// Winner: stream the buffered first event + the rest live.
		out := make(chan Event, 16)
		go func() {
			out <- first
			for ev := range ch {
				out <- ev
			}
			close(out)
		}()
		return out, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("fallback: all providers exhausted")
	}
	return nil, lastErr
}

// nextEligible returns the index of the next step after `cur` whose Condition
// matches err (and err is retryable), or `cur` if none is eligible (meaning:
// stop and surface the error).
func (f *FallbackProvider) nextEligible(cur int, err error, retry func(error) bool) int {
	if !retry(err) {
		return cur
	}
	for j := cur + 1; j < len(f.Steps); j++ {
		if conditionMatches(f.Steps[j].Condition, err) {
			return j
		}
	}
	return cur
}

// conditionMatches reports whether a fallback edge's condition applies to err.
// An empty condition (or "always"/"any") matches any error; named conditions
// match the corresponding error class so a chain can route (e.g.) rate-limit
// errors down one path and context-overflow down another.
func conditionMatches(condition string, err error) bool {
	if err == nil {
		return false
	}
	c := strings.ToLower(strings.TrimSpace(condition))
	switch c {
	case "", "always", "any", "error", "*":
		return true
	}
	m := strings.ToLower(err.Error())
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch c {
	case "rate_limit", "ratelimit", "429":
		return has("429", "rate limit", "rate_limit", "too many requests", "overloaded", "quota")
	case "auth", "unauthorized", "401", "403":
		return has("401", "403", "unauthorized", "forbidden", "api key", "authentication", "not set", "no api key")
	case "context_overflow", "context_length", "context", "overflow":
		return has("context length", "context_length", "maximum context", "too many tokens", "context window", "reduce the length")
	case "timeout", "deadline":
		return has("timeout", "deadline", "timed out")
	case "server_error", "5xx", "500", "502", "503":
		return has("500", "502", "503", "server error", "overloaded", "unavailable", "bad gateway")
	case "bad_request", "400", "invalid_model", "invalid":
		return has("400", "bad request", "invalid model", "invalid_model", "model not found", "does not exist")
	default:
		// Unknown condition string — be permissive so a misconfigured edge still
		// participates rather than silently blocking the whole chain.
		return true
	}
}

// DefaultRetryable matches the error classes worth failing over on: the ones
// hermes-octo skips on (429/401/400) plus transient network/server errors.
// Deterministic request errors that would fail on every provider identically
// (e.g. malformed JSON) are intentionally excluded so we don't burn the chain.
func DefaultRetryable(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, p := range []string{
		"429", "rate limit", "rate_limit", "too many requests", "quota",
		"401", "unauthorized", "api key", "authentication", "not set",
		"403", "forbidden",
		"400", "bad request", "invalid model", "model not found", "does not exist",
		"context length", "context_length", "maximum context", "context window",
		"timeout", "deadline", "timed out",
		"500", "502", "503", "server error", "bad gateway", "unavailable",
		"overloaded", "temporarily", "connection refused", "no such host", "eof",
	} {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

func drainEvents(ch <-chan Event) {
	for range ch {
	}
}
