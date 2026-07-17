// Package paths resolves TAG state directories (Go port of core/paths.py).
package paths

import (
	"os"
	"path/filepath"
)

const (
	AppName            = "TAG"
	CLILabel           = "tag"
	defaultHermesCkout = "managed/hermes-agent-upstream"
)

// Home returns the resolved TAG_HOME (env TAG_HOME or ~/.tag).
func Home() string {
	if v := os.Getenv("TAG_HOME"); v != "" {
		abs, err := filepath.Abs(expand(v))
		if err == nil {
			return abs
		}
		return expand(v)
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".tag")
}

// Expand resolves a leading "~" (bare, "~/…") to the user's home dir, matching
// Python's Path.expanduser(). "~user" is left as-is (rare; not supported).
func Expand(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if len(p) >= 2 && p[:2] == "~/" {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func expand(p string) string { return Expand(p) }

// ConfigRoot is TAG_HOME/config.
func ConfigRoot() string { return filepath.Join(Home(), "config") }

// ConfigFile is the default tag.yaml path.
func ConfigFile() string { return filepath.Join(ConfigRoot(), "tag.yaml") }

// ManagedRoot is TAG_HOME/managed.
func ManagedRoot() string { return filepath.Join(Home(), "managed") }

// ResolveHomeRelative resolves a config value relative to TAG_HOME unless absolute.
func ResolveHomeRelative(v string) string {
	v = expand(v)
	if filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(Home(), v)
}

// RuntimeHome resolves runtime.home_dir (default runtime/home).
func RuntimeHome(homeDir string) string {
	if homeDir == "" {
		homeDir = "runtime/home"
	}
	return ResolveHomeRelative(homeDir)
}

// RuntimeDBPath resolves runtime.db_path (default runtime/tag.sqlite3).
func RuntimeDBPath(dbPath string) string {
	if dbPath == "" {
		dbPath = "runtime/tag.sqlite3"
	}
	return ResolveHomeRelative(dbPath)
}

// ProfileHome is runtime_home/.hermes/profiles/<name>.
func ProfileHome(homeDir, profile string) string {
	return filepath.Join(RuntimeHome(homeDir), ".hermes", "profiles", profile)
}

// EnsureRuntimeDirs makes the runtime dir tree.
func EnsureRuntimeDirs(homeDir, dbPath string) error {
	if err := os.MkdirAll(RuntimeHome(homeDir), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Dir(RuntimeDBPath(dbPath)), 0o755)
}
