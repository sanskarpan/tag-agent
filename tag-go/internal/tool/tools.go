// Package tool provides the built-in local tools the native agent loop executes
// (Track B). Each tool is provider-neutral and side-effecting on the local host;
// they plug into agent.Registry. All are testable offline (no model calls).
package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// Options bounds tool side effects.
type Options struct {
	// Root confines file tools to this directory (path-traversal guard). Empty = cwd.
	Root string
	// BashTimeout caps shell command runtime.
	BashTimeout time.Duration
	// MaxReadBytes caps read_file output.
	MaxReadBytes int64
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
	reg.Add(bashTool(opts))
	reg.Add(readFileTool(opts))
	reg.Add(writeFileTool(opts))
	reg.Add(listDirTool(opts))
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

func bashTool(opts Options) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{
			Name:        "bash",
			Description: "Run a shell command and return combined stdout+stderr.",
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
			out, err := c.CombinedOutput()
			if cctx.Err() == context.DeadlineExceeded {
				return string(out), fmt.Errorf("command timed out after %s", opts.BashTimeout)
			}
			if err != nil {
				return string(out), fmt.Errorf("exit error: %v", err)
			}
			return string(out), nil
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
