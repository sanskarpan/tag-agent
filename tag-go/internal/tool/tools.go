// Package tool provides the built-in local tools the native agent loop executes
// (Track B). Each tool is provider-neutral and side-effecting on the local host;
// they plug into agent.Registry. All are testable offline (no model calls).
package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/permission"
	"github.com/tag-agent/tag/internal/sandbox"
)

// Options bounds tool side effects.
type Options struct {
	// Root confines file tools to this directory (path-traversal guard). Empty = cwd.
	Root string
	// BashTimeout caps shell command runtime.
	BashTimeout time.Duration
	// DisableBash omits the bash tool, leaving only the root-confined file tools.
	// The bash tool executes unrestricted host commands (Root only sets its
	// working directory; it is not a sandbox).
	DisableBash bool
	// MaxReadBytes caps how much output this package hands back to the model:
	// read_file's content AND bash's combined stdout+stderr. Both come from
	// model-controlled input (a path, a command line), so both need a ceiling;
	// bash used to have none. A bash result that hit the cap says so explicitly
	// rather than being silently shortened.
	MaxReadBytes int64
	// Disabled is a tool-budget allowlist gate (hermes-octo trims ~20 toolsets by
	// default and flips them on for deep sessions). A tool whose Def.Name is a key
	// here is omitted from Register, keeping the model's tool list lean.
	Disabled map[string]bool
	// EnableExa turns on the Exa `web_search` tool (off by default — it needs an
	// API key and adds to the tool budget).
	EnableExa bool
	// ExaAPIKey overrides EXA_API_KEY (mostly for tests). ExaBaseURL overrides the
	// Exa endpoint (default https://api.exa.ai); ExaClient overrides the HTTP client.
	ExaAPIKey  string
	ExaBaseURL string
	ExaClient  *http.Client
	// Guard is the consent gate every registered tool is routed through. A nil
	// Guard does NOT mean "ungated": Register substitutes permission's secure
	// default policy with no prompter, which denies bash and write_file. Callers
	// that want interaction or config-driven rules must pass a real Guard.
	Guard *permission.Guard
}

// guard returns the effective consent gate: the caller's, or a fail-safe
// default (secure built-in policy, no prompter => `ask` resolves to deny).
func (o Options) guard() *permission.Guard {
	if o.Guard != nil {
		return o.Guard
	}
	return permission.NewGuard(permission.DefaultPolicy(), nil, nil)
}

func (o Options) exaKey() string {
	if o.ExaAPIKey != "" {
		return o.ExaAPIKey
	}
	return os.Getenv("EXA_API_KEY")
}

// DefaultOptions returns safe defaults.
func DefaultOptions() Options {
	return Options{BashTimeout: 30 * time.Second, MaxReadBytes: 256 * 1024}
}

// Register adds the built-in tools to a registry.
func Register(reg *agent.Registry, opts Options) {
	if opts.BashTimeout == 0 {
		opts.BashTimeout = 30 * time.Second
	}
	if opts.MaxReadBytes == 0 {
		opts.MaxReadBytes = 256 * 1024
	}
	g := opts.guard()
	// add applies the tool-budget gate (Options.Disabled) AND the permission gate
	// uniformly. Every built-in tool goes through here, so there is no path that
	// registers an ungated tool.
	add := func(t agent.Tool, subject permission.SubjectFunc) {
		if opts.Disabled[t.Def.Name] {
			return
		}
		reg.Add(permission.Wrap(g, t, subject))
	}
	if !opts.DisableBash {
		add(bashTool(opts), commandSubject)
	}
	add(readFileTool(opts), pathSubject(opts, "path"))
	add(writeFileTool(opts), pathSubject(opts, "path"))
	add(listDirTool(opts), pathSubject(opts, "path"))
	if opts.EnableExa && opts.exaKey() != "" {
		add(exaSearchTool(opts), permission.NoSubject)
	}
}

// commandSubject exposes the bash command line to the permission gate.
func commandSubject(in map[string]any) (permission.Kind, string) {
	return permission.KindCommand, strArg(in, "command")
}

// toolRoot returns the directory the file tools are confined to, absolute and
// with its OWN symlinks resolved (e.g. macOS /tmp -> /private/tmp), so every
// later comparison is between real paths. Shared by pathSubject and resolvePath
// precisely so the path the gate adjudicates and the path the tool opens are
// derived identically.
func toolRoot(opts Options) string {
	root := opts.Root
	if root == "" {
		root, _ = os.Getwd()
	}
	root, _ = filepath.Abs(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root
}

// lexicalPath turns a tool's `path` argument into an absolute, cleaned path
// under the tool root, WITHOUT touching the filesystem.
func lexicalPath(opts Options, rel string) string {
	if rel == "" {
		rel = "."
	}
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(toolRoot(opts), rel)
	}
	return filepath.Clean(p)
}

// realPath resolves every symlink in p and returns the result.
//
// The target (and any intermediate dirs) may not exist yet — write_file creates
// parents — so it walks UP to the deepest ancestor that DOES exist, resolves
// THAT with EvalSymlinks, and re-attaches the not-yet-existing tail. A path
// whose components all exist therefore comes back fully dereferenced, and a path
// being created comes back rooted at its real parent.
//
// It fails CLOSED: when a component that we know exists cannot be resolved (a
// dangling or looping link), it returns an error rather than a path that has
// silently skipped the check.
func realPath(p string) (string, error) {
	check := p
	var tail []string
	for {
		if _, err := os.Lstat(check); err == nil {
			break // deepest existing ancestor found
		}
		parent := filepath.Dir(check)
		if parent == check {
			// Walked to the filesystem root without finding an existing ancestor;
			// there is nothing to dereference.
			return p, nil
		}
		tail = append(tail, filepath.Base(check))
		check = parent
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", fmt.Errorf("path %q could not be resolved: %w", p, err)
	}
	for i := len(tail) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, tail[i])
	}
	return resolved, nil
}

// pathSubject exposes a file tool's target to the permission gate as an
// ABSOLUTE, cleaned, SYMLINK-RESOLVED path.
//
// Both halves matter, and each closes a different hole:
//
//   - Cleaning to an absolute path is what makes "../../.ssh/id_rsa" match the
//     ~/.ssh/** deny rule instead of sliding past a basename check.
//   - Resolving symlinks is what stops an in-root link with an innocuous name
//     from laundering a denied file. The tools OPEN the dereferenced path, so
//     adjudicating the lexical one meant the gate and the syscall disagreed:
//     `notes.txt -> secrets.env` was matched as "notes.txt" (allowed), then read
//     as "secrets.env", bypassing the builtin *.env / *.pem / ~/.ssh/** denies.
//     Anything that can create a symlink in the workspace — including the
//     model's own bash tool — could mint one.
//
// Resolution is best-effort ON PURPOSE. resolvePath rejects a traversal escape
// or an unresolvable link, but a rule must still be able to SEE (and deny) the
// path the model asked for, so when resolution fails the lexically cleaned form
// is adjudicated instead. That is never a widening: the lexical form is what
// this function returned before, and the tool then refuses the call anyway.
func pathSubject(opts Options, key string) permission.SubjectFunc {
	return func(in map[string]any) (permission.Kind, string) {
		p := lexicalPath(opts, strArg(in, key))
		if resolved, err := realPath(p); err == nil {
			return permission.KindPath, resolved
		}
		return permission.KindPath, p
	}
}

// resolvePath confines rel to opts.Root (or cwd) and returns the DEREFERENCED
// path the tool should operate on. It rejects traversal escapes AND symlinks
// that point outside the root (a lexical prefix check alone is not enough — a
// symlink inside the root can target /etc/passwd).
//
// It also performs the POST-RESOLUTION PERMISSION RE-CHECK. pathSubject already
// adjudicated the resolved path before the tool was entered; this second look
// closes the TOCTOU window in between, where a symlink is swapped after the
// verdict and before the open. The re-check consults the ruleset only
// (Guard.StaticDeny): it never prompts, never records, and can only turn an
// allow into a deny, so it cannot re-ask a human or widen a verdict.
func resolvePath(opts Options, tool, rel string) (string, error) {
	root := toolRoot(opts)
	p := lexicalPath(opts, rel)
	// Lexical guard first (catches `..` before any filesystem access).
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the tool root", rel)
	}
	real, err := realPath(p)
	if err != nil {
		return "", fmt.Errorf("path %q could not be resolved for the tool root check: %w", rel, err)
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q resolves outside the tool root via a symlink", rel)
	}
	req := permission.Request{Tool: tool, Kind: permission.KindPath, Subject: real}
	if rule, denied := opts.guard().StaticDeny(req); denied {
		return "", &permission.DeniedError{Request: req, Decision: permission.Decision{
			Action: permission.Deny, Rule: rule, Via: "rule",
			Reason: fmt.Sprintf("%s resolves to %s, which is denied by rule %s", strconv.Quote(rel), real, rule.String()),
		}}
	}
	return real, nil
}

func strArg(in map[string]any, key string) string {
	if v, ok := in[key].(string); ok {
		return v
	}
	return ""
}

// bashWaitDelay bounds how long Wait may block on the output pipe after the
// process itself is gone — a backgrounded grandchild inherits that pipe and
// would otherwise hold Wait open indefinitely.
const bashWaitDelay = 2 * time.Second

// capWriter is an io.Writer that keeps at most Max bytes and silently discards
// the rest, remembering how much it dropped.
//
// The bash tool needs it because the command line comes from model output and
// the capture had no ceiling: `cat /dev/zero`, `yes`, or a stray `find /` grew a
// bytes.Buffer until the process died. Discarding rather than erroring is
// deliberate — the command still ran, and the first N bytes are usually the
// useful part — but the result SAYS it was truncated, so neither the model nor
// the operator is shown a silently shortened output as if it were complete.
type capWriter struct {
	Max     int
	buf     []byte
	dropped int
}

func (w *capWriter) Write(p []byte) (int, error) {
	kept := 0
	if room := w.Max - len(w.buf); room > 0 {
		kept = min(room, len(p))
		w.buf = append(w.buf, p[:kept]...)
	}
	w.dropped += len(p) - kept
	// Always report a full write. A short write is an error to exec's copier and
	// would tear down the pipe (and the command) instead of just capping what we
	// keep; we want the command to run to completion, just not to be stored.
	return len(p), nil
}

// String renders the captured output, appending an honest truncation notice.
func (w *capWriter) String() string {
	s := string(w.buf)
	if w.dropped > 0 {
		s += fmt.Sprintf("\n\n[output truncated: %d more bytes were produced and discarded "+
			"(limit %d bytes)]", w.dropped, w.Max)
	}
	return s
}

func bashTool(opts Options) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{
			Name:        "bash",
			Description: "Run a shell command and return combined stdout+stderr. Commands execute unrestricted on the host (NOT confined to the tool root, unlike the file tools); the tool root is only the working directory when one is configured.",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}},
		},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			cmdStr := strArg(in, "command")
			if strings.TrimSpace(cmdStr) == "" {
				return "", fmt.Errorf("command is required")
			}
			cctx, cancel := context.WithTimeout(ctx, opts.BashTimeout)
			defer cancel()
			c := exec.CommandContext(cctx, "sh", "-c", cmdStr)
			if opts.Root != "" {
				c.Dir = opts.Root
			}
			// The timeout must bind the whole process tree, not just `sh`.
			// CommandContext's default kill reaches only the direct child, so a
			// backgrounded grandchild (`sleep 40 & wait`, `npm run dev &`) survives
			// AND keeps the output pipe open — CombinedOutput then blocks until the
			// grandchild exits, i.e. 40s for a 2s cap and forever for an unbounded
			// child. That is reachable straight from untrusted model output.
			// Same pattern as internal/sandbox: own process group + group-wide
			// SIGKILL on cancel + a WaitDelay so a lingering pipe-holder cannot
			// stall Wait.
			sandbox.SetProcGroup(c, nil)
			c.Cancel = func() error { return sandbox.KillProcessGroup(c) }
			c.WaitDelay = bashWaitDelay

			// Bounded capture: opts.MaxReadBytes is the ceiling on how much tool
			// output this package ever hands back to the model, for bash exactly as
			// for read_file. Without it the buffer grew without limit.
			buf := &capWriter{Max: int(opts.MaxReadBytes)}
			c.Stdout = buf
			c.Stderr = buf
			if err := c.Start(); err != nil {
				return "", fmt.Errorf("exit error: %v", err)
			}
			runErr := c.Wait()
			// Reap survivors even on the happy path: a grandchild that outlives its
			// parent would otherwise be re-parented to init and keep running.
			_ = sandbox.KillProcessGroup(c)
			out := buf.String()
			if cctx.Err() == context.DeadlineExceeded {
				return out, fmt.Errorf("command timed out after %s", opts.BashTimeout)
			}
			if runErr != nil {
				// ErrWaitDelay means the command itself finished but something it
				// spawned held the pipe past the delay. Report the real exit status.
				if errors.Is(runErr, exec.ErrWaitDelay) {
					if c.ProcessState != nil && c.ProcessState.ExitCode() == 0 {
						return out, nil
					}
				}
				return out, fmt.Errorf("exit error: %v", runErr)
			}
			return out, nil
		},
	}
}

func readFileTool(opts Options) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{
			Name:        "read_file",
			Description: "Read a UTF-8 text file (confined to the tool root).",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}},
		},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			p, err := resolvePath(opts, "read_file", strArg(in, "path"))
			if err != nil {
				return "", err
			}
			f, err := os.Open(p)
			if err != nil {
				return "", err
			}
			defer f.Close()
			// Read up to MaxReadBytes; a single f.Read may short-read, so drain
			// via io.ReadAll on a bounded reader.
			b, err := io.ReadAll(io.LimitReader(f, opts.MaxReadBytes))
			if err != nil {
				return "", err
			}
			// A byte cap can land mid-rune, which would make an ordinary text file
			// look like invalid UTF-8 and be refused as binary. Trim before the
			// check, not after it.
			truncated := int64(len(b)) == opts.MaxReadBytes
			if truncated {
				b = trimPartialRune(b)
			}
			// The tool promises a UTF-8 text file. Handing back a PDF's bytes and
			// reporting success is the fabricated-success pattern, and an expensive
			// one: ~100k tokens of deflate output before the model can discover it
			// has nothing. See textcheck.go for why the checks are conservative.
			if reason := notTextReason(b); reason != "" {
				size := int64(len(b))
				if st, serr := f.Stat(); serr == nil {
					size = st.Size()
				}
				return "", binaryRefusal(strArg(in, "path"), reason, size)
			}
			// Truncation has to be stated even for text. Returning 29% of a file and
			// reporting success is the same broken guarantee in a quieter form.
			if truncated {
				if st, serr := f.Stat(); serr == nil && st.Size() > opts.MaxReadBytes {
					// The notice goes FIRST and is phrased as an instruction. The
					// only way to edit a file here is read-whole then write-whole
					// (write_file takes full content), so a trailing marker invites
					// the model to write the truncated content back and destroy the
					// unread remainder -- turning a read-side limit into data loss.
					return fmt.Sprintf(
						"%s\n\n%s",
						truncationNotice(opts.MaxReadBytes, st.Size()),
						string(b)), nil
				}
			}
			return string(b), nil
		},
	}
}

func writeFileTool(opts Options) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{
			Name:        "write_file",
			Description: "Write text to a file (confined to the tool root; creates parent dirs).",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"path", "content"}},
		},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			p, err := resolvePath(opts, "write_file", strArg(in, "path"))
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			content := strArg(in, "content")
			// Refuse to write back a truncated read. The only edit path here is
			// read-whole then write-whole, so a model that reads a >MaxReadBytes
			// file and writes it back would destroy everything past the cap and
			// inject the notice into the source. The read side warns; this side
			// makes the warning enforceable.
			if strings.Contains(content, TruncationMarker) {
				return "", fmt.Errorf(
					"write_file: refusing to write content that contains the read_file " +
						"truncation notice — it is an INCOMPLETE read of a larger file, and " +
						"writing it back would discard everything past the cap")
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(content), p), nil
		},
	}
}

func listDirTool(opts Options) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{
			Name:        "list_dir",
			Description: "List entries in a directory (confined to the tool root).",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			rel := strArg(in, "path")
			if rel == "" {
				rel = "."
			}
			p, err := resolvePath(opts, "list_dir", rel)
			if err != nil {
				return "", err
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return "", err
			}
			var names []string
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			return strings.Join(names, "\n"), nil
		},
	}
}
