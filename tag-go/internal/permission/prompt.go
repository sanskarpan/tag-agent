package permission

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// IsTerminal reports whether f is an interactive character device. This is the
// ONLY thing that decides whether `ask` can reach a human; everything headless
// (queue worker, cron daemon, dag run --execute, gateway, CI) lands here as
// false and gets the fail-closed path.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// StdioInteractive reports whether BOTH stdin and stderr are terminals, i.e.
// whether we can print a prompt and read an answer.
func StdioInteractive() bool {
	return IsTerminal(os.Stdin) && IsTerminal(os.Stderr)
}

// TTYPrompter asks the operator on a real terminal. It writes the prompt to
// Out (stderr by default, so it never pollutes --json stdout) and reads a line
// from In.
type TTYPrompter struct {
	In  io.Reader
	Out io.Writer

	reader *bufio.Reader
}

// NewTTYPrompter builds a prompter over the given streams.
func NewTTYPrompter(in io.Reader, out io.Writer) *TTYPrompter {
	return &TTYPrompter{In: in, Out: out}
}

// Ask prints the tool and its concrete arguments and reads a verdict.
// Answers: y = allow once, a = allow for the session, n / anything else = deny.
// EOF or a read error is a DENY, never an approval.
func (p *TTYPrompter) Ask(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return ResponseDeny, err
	}
	out := p.Out
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "\n  permission required: tool %q\n", req.Tool)
	switch req.Kind {
	case KindPath:
		fmt.Fprintf(out, "    path:    %s\n", req.Subject)
	case KindCommand:
		fmt.Fprintf(out, "    command: %s\n", req.Subject)
	}
	for _, line := range extraArgLines(req) {
		fmt.Fprintf(out, "    %s\n", line)
	}
	fmt.Fprintf(out, "    [y] allow once  [a] allow for this session  [n] deny (default): ")

	if p.reader == nil {
		in := p.In
		if in == nil {
			in = os.Stdin
		}
		p.reader = bufio.NewReader(in)
	}
	line, err := p.reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// EOF / closed stdin -> deny. Do NOT treat "no answer" as consent.
		fmt.Fprintln(out, "n (no input)")
		return ResponseDeny, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return ResponseAllowOnce, nil
	case "a", "always", "session":
		return ResponseAllowSession, nil
	default:
		return ResponseDeny, nil
	}
}

// extraArgLines renders the non-subject arguments compactly (truncated), so the
// human sees what is actually about to happen without a wall of text.
func extraArgLines(req Request) []string {
	var out []string
	for _, k := range []string{"content"} {
		if v, ok := req.Args[k].(string); ok {
			out = append(out, fmt.Sprintf("%s:  %s (%d bytes)", k, truncate(v, 120), len(v)))
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", "\\n"), "\r", "")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// FuncPrompter adapts a function to the Prompter interface (used by tests so no
// real terminal is ever needed).
type FuncPrompter func(ctx context.Context, req Request) (Response, error)

// Ask implements Prompter.
func (f FuncPrompter) Ask(ctx context.Context, req Request) (Response, error) { return f(ctx, req) }
