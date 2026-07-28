// Package tool provides the built-in local tools the native agent loop executes
// (Track B). Each tool is provider-neutral and side-effecting on the local host;
// they plug into agent.Registry. All are testable offline (no model calls).
package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	// MaxReadBytes caps read_file output.
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

// pathSubject exposes a file tool's target as an ABSOLUTE, lexically cleaned
// path. It deliberately does NOT use resolvePath: that fails on a traversal
// escape, and a rule must still be able to see (and deny) the path the model
// asked for. Cleaning first is what makes "../../.ssh/id_rsa" match the
// ~/.ssh/** deny rule instead of sliding past a basename check.
func pathSubject(opts Options, key string) permission.SubjectFunc {
	return func(in map[string]any) (permission.Kind, string) {
		rel := strArg(in, key)
		if rel == "" {
			rel = "."
		}
		root := opts.Root
		if root == "" {
			root, _ = os.Getwd()
		}
		root, _ = filepath.Abs(root)
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, rel)
		}
		return permission.KindPath, filepath.Clean(p)
	}
}

// resolvePath confines rel to opts.Root (or cwd), rejecting traversal escapes
// AND symlinks that point outside the root (a lexical prefix check alone is not
// enough — a symlink inside the root can target /etc/passwd).
func resolvePath(opts Options, rel string) (string, error) {
	root := opts.Root
	if root == "" {
		root, _ = os.Getwd()
	}
	root, _ = filepath.Abs(root)
	// Resolve any symlinks in the root itself so comparisons use real paths
	// (e.g. macOS /tmp -> /private/tmp).
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = resolvedRoot
	}
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, rel)
	}
	p = filepath.Clean(p)
	// Lexical guard first (catches `..` before any filesystem access).
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the tool root", rel)
	}
	// Symlink guard: the target (and any intermediate dirs) may not exist yet
	// (write_file creates parents), so walk UP to the deepest ancestor that DOES
	// exist and resolve that. A symlinked ancestor pointing outside the root —
	// even one whose deeper components don't exist yet — is rejected. Fail CLOSED:
	// if EvalSymlinks errors on a path we know exists, treat it as an escape
	// rather than skipping the check.
	check := p
	for {
		if _, err := os.Lstat(check); err == nil {
			break // deepest existing ancestor found
		}
		parent := filepath.Dir(check)
		if parent == check {
			// Walked to the filesystem root without finding an existing ancestor;
			// nothing to resolve (the lexical guard already vetted the path).
			return p, nil
		}
		check = parent
	}
	real, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", fmt.Errorf("path %q could not be resolved for the tool root check: %w", rel, err)
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q resolves outside the tool root via a symlink", rel)
	}
	return p, nil
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

			var buf bytes.Buffer
			c.Stdout = &buf
			c.Stderr = &buf
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
			p, err := resolvePath(opts, strArg(in, "path"))
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
			p, err := resolvePath(opts, strArg(in, "path"))
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			content := strArg(in, "content")
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
			p, err := resolvePath(opts, rel)
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
