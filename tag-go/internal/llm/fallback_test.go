package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockProvider is a test provider that either errors at Stream time, emits an
// EventError first, or streams a fixed text. It records whether it was called.
type mockProvider struct {
	name      string
	streamErr error  // returned from Stream() before any channel
	eventErr  error  // emitted as the first EventError on the channel
	text      string // streamed as text when no error
	called    *bool
	gotModel  *string
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if m.called != nil {
		*m.called = true
	}
	if m.gotModel != nil {
		*m.gotModel = req.Model
	}
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	ch := make(chan Event, 4)
	go func() {
		defer close(ch)
		if m.eventErr != nil {
			ch <- Event{Type: EventError, Err: m.eventErr}
			return
		}
		ch <- Event{Type: EventTextDelta, Text: m.text}
		ch <- Event{Type: EventFinish}
	}()
	return ch, nil
}

func collectText(t *testing.T, ch <-chan Event) (string, error) {
	t.Helper()
	var sb strings.Builder
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			sb.WriteString(ev.Text)
		case EventError:
			return sb.String(), ev.Err
		}
	}
	return sb.String(), nil
}

func TestFallbackPrimarySucceeds(t *testing.T) {
	var p2called bool
	f := &FallbackProvider{Steps: []FallbackStep{
		{Provider: &mockProvider{name: "a", text: "primary"}},
		{Provider: &mockProvider{name: "b", text: "backup", called: &p2called}},
	}}
	ch, err := f.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got, _ := collectText(t, ch)
	if got != "primary" {
		t.Errorf("want primary, got %q", got)
	}
	if p2called {
		t.Error("backup must NOT be called when primary succeeds")
	}
}

func TestFallbackOnStreamError(t *testing.T) {
	// primary errors at Stream() with a 429; chain must advance to backup.
	var p2model string
	f := &FallbackProvider{Steps: []FallbackStep{
		{Provider: &mockProvider{name: "a", streamErr: errors.New("openai API 429: rate limit exceeded")}},
		{Provider: &mockProvider{name: "b", text: "backup ok", gotModel: &p2model}, Model: "openrouter/free"},
	}}
	ch, err := f.Stream(context.Background(), Request{Model: "openai/gpt-4o"})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	got, _ := collectText(t, ch)
	if got != "backup ok" {
		t.Errorf("want backup ok, got %q", got)
	}
	if p2model != "openrouter/free" {
		t.Errorf("backup step must use its own model, got %q", p2model)
	}
}

func TestFallbackOnEventError(t *testing.T) {
	// primary emits an EventError (401) before content; chain advances.
	f := &FallbackProvider{Steps: []FallbackStep{
		{Provider: &mockProvider{name: "a", eventErr: errors.New("401 unauthorized: invalid api key")}},
		{Provider: &mockProvider{name: "b", text: "recovered"}},
	}}
	ch, err := f.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got, _ := collectText(t, ch)
	if got != "recovered" {
		t.Errorf("want recovered, got %q", got)
	}
}

func TestFallbackNonRetryableStops(t *testing.T) {
	// A non-retryable error (deterministic bad JSON) must NOT fall back.
	var p2called bool
	f := &FallbackProvider{Steps: []FallbackStep{
		{Provider: &mockProvider{name: "a", streamErr: errors.New("json: cannot unmarshal number")}},
		{Provider: &mockProvider{name: "b", text: "backup", called: &p2called}},
	}}
	_, err := f.Stream(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected the non-retryable error to surface")
	}
	if p2called {
		t.Error("must NOT fall back on a non-retryable error")
	}
}

func TestFallbackConditionGating(t *testing.T) {
	// The backup is gated on condition=rate_limit but the error is a 401 (auth):
	// the condition does not match, so the chain must stop, not use the backup.
	var p2called bool
	f := &FallbackProvider{Steps: []FallbackStep{
		{Provider: &mockProvider{name: "a", streamErr: errors.New("401 unauthorized")}},
		{Provider: &mockProvider{name: "b", text: "backup", called: &p2called}, Condition: "rate_limit"},
	}}
	_, err := f.Stream(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected the 401 to surface, got %v", err)
	}
	if p2called {
		t.Error("backup gated on rate_limit must NOT handle a 401")
	}
}

func TestFallbackAllExhausted(t *testing.T) {
	var idx []int
	f := &FallbackProvider{
		Steps: []FallbackStep{
			{Provider: &mockProvider{name: "a", streamErr: errors.New("429 rate limit")}},
			{Provider: &mockProvider{name: "b", streamErr: errors.New("503 unavailable")}},
		},
		OnFallback: func(i int, model string, err error) { idx = append(idx, i) },
	}
	_, err := f.Stream(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected the last error to surface, got %v", err)
	}
	if len(idx) != 1 || idx[0] != 0 {
		t.Errorf("OnFallback should fire once for step 0, got %v", idx)
	}
}

func TestConditionMatches(t *testing.T) {
	cases := []struct {
		cond string
		err  string
		want bool
	}{
		{"", "anything", true},
		{"always", "x", true},
		{"rate_limit", "openai API 429: rate limit", true},
		{"rate_limit", "401 unauthorized", false},
		{"auth", "401 unauthorized", true},
		{"context_overflow", "maximum context length exceeded", true},
		{"timeout", "context deadline exceeded", true},
		{"server_error", "503 service unavailable", true},
	}
	for _, c := range cases {
		if got := conditionMatches(c.cond, errors.New(c.err)); got != c.want {
			t.Errorf("conditionMatches(%q, %q) = %v, want %v", c.cond, c.err, got, c.want)
		}
	}
}
