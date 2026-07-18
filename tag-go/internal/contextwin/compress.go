package contextwin

import (
	"fmt"
	"strings"
)

// TokensPerChar is the rough chars->tokens divisor used for the offline token
// estimate (mirrors the ~4 chars/token heuristic used across the Go port, e.g.
// llm.EchoProvider's usage accounting).
const TokensPerChar = 4

// Item is one unit of stored session context assembled for compression/trim.
// It is intentionally minimal — a role + text — so it can be sourced from runs,
// steps, or memory rows uniformly.
type Item struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// EstimateTokens returns the rough token estimate for a string (len/4, min 0).
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s) / TokensPerChar
}

// TotalTokens sums the estimated tokens across items.
func TotalTokens(items []Item) int {
	total := 0
	for _, it := range items {
		total += EstimateTokens(it.Text)
	}
	return total
}

// KeepLast returns the last n items (the most-recent turns). n<=0 yields an
// empty slice; n>=len returns all items. This is the pure core of `context
// trim --keep-last N`.
func KeepLast(items []Item, n int) []Item {
	if n <= 0 {
		return []Item{}
	}
	if n >= len(items) {
		out := make([]Item, len(items))
		copy(out, items)
		return out
	}
	out := make([]Item, n)
	copy(out, items[len(items)-n:])
	return out
}

// Transcript renders items into a single prompt string for the summarizer. Each
// item is one "role: text" line, so the agent loop sees the whole session as a
// flat conversation to compress.
func Transcript(items []Item) string {
	var b strings.Builder
	for _, it := range items {
		role := it.Role
		if role == "" {
			role = "message"
		}
		fmt.Fprintf(&b, "%s: %s\n", role, it.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SummaryPrompt wraps a transcript in a summarization instruction. The offline
// echo provider echoes the last user message verbatim, so the instruction is
// phrased to make the echoed text a self-describing compressed record.
func SummaryPrompt(transcript string) string {
	if transcript == "" {
		return "Summarize the conversation so far into a concise compressed context. (empty session)"
	}
	return "Summarize the following conversation into a concise compressed context, " +
		"preserving key facts, decisions, and open questions:\n\n" + transcript
}
