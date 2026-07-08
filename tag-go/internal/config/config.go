// Package config loads and atomically persists the TAG config (Go port of core/config.py).
package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"github.com/tag-agent/tag/internal/paths"
	yaml "gopkg.in/yaml.v3"
)

//go:embed assets/default.yaml
var defaultYAML []byte

//go:embed assets/mcp-registry.yaml
var mcpRegistryYAML []byte

// MCPRegistry returns the bundled MCP server catalog (servers map).
func MCPRegistry() (map[string]any, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(mcpRegistryYAML, &doc); err != nil {
		return nil, err
	}
	if servers, ok := doc["servers"].(map[string]any); ok {
		return servers, nil
	}
	return map[string]any{}, nil
}

//go:embed assets/plugin-registry.yaml
var pluginRegistryYAML []byte

// PluginRegistry returns the bundled plugin catalog (plugins.registry map).
func PluginRegistry() (map[string]any, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(pluginRegistryYAML, &doc); err != nil {
		return nil, err
	}
	if plugins, ok := doc["plugins"].(map[string]any); ok {
		if reg, ok := plugins["registry"].(map[string]any); ok {
			return reg, nil
		}
	}
	return map[string]any{}, nil
}

// Config is the dynamic TAG config tree.
type Config struct {
	Data map[string]any
	Path string
}

// Path returns the effective config file path (override or default), seeding the
// default file on first use.
func Path(override string) (string, error) {
	if override != "" {
		// Expand a leading ~ (matching Python's expanduser) before resolving.
		abs, err := filepath.Abs(paths.Expand(override))
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	p := paths.ConfigFile()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, defaultYAML, 0o644); err != nil {
			return "", err
		}
	}
	return p, nil
}

// Load reads and parses the config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
		return nil, fmt.Errorf("config at %s could not be read: %w", path, err)
	}
	// Decode into a generic value first so malformed YAML surfaces a parse error
	// (#536) and a valid-but-non-object document (top-level list/scalar) yields a
	// clean "must be a YAML object" message, matching Python's load_config.
	var raw any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("config at %s is not valid YAML: %w", path, err)
	}
	if raw == nil {
		return &Config{Data: map[string]any{}, Path: path}, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config at %s must be a YAML object", path)
	}
	return &Config{Data: m, Path: path}, nil
}

// LoadDefault loads from the override or the seeded default path.
func LoadDefault(override string) (*Config, error) {
	p, err := Path(override)
	if err != nil {
		return nil, err
	}
	return Load(p)
}

// Save atomically persists the config under an advisory lock (fixes the B005 race).
func Save(path string, data map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	out, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	tmp.Close()
	return os.Rename(tmpName, path)
}

// Update performs a locked read-modify-write cycle (Go port of update_config).
func Update(path string, mutate func(map[string]any)) (map[string]any, error) {
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	mutate(cfg.Data)
	// write within the same lock (reuse Save body without re-locking)
	out, err := yaml.Marshal(cfg.Data)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	tmp.Sync()
	tmp.Close()
	if err := os.Rename(tmpName, path); err != nil {
		return nil, err
	}
	return cfg.Data, nil
}

// Section returns a nested map[string]any for a top-level key (nil-safe).
func (c *Config) Section(key string) map[string]any {
	if c == nil || c.Data == nil {
		return map[string]any{}
	}
	if v, ok := c.Data[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// String returns a nested string via dotted path.
func (c *Config) String(dotted, def string) string {
	v := c.get(dotted)
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func (c *Config) get(dotted string) any {
	if c == nil {
		return nil
	}
	cur := any(c.Data)
	start := 0
	for i := 0; i <= len(dotted); i++ {
		if i == len(dotted) || dotted[i] == '.' {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[dotted[start:i]]
			start = i + 1
		}
	}
	return cur
}

// Profiles returns the profiles map (nil-safe).
func (c *Config) Profiles() map[string]any { return c.Section("profiles") }

// MasterProfile returns defaults.master_profile or "orchestrator".
func (c *Config) MasterProfile() string {
	if p := c.String("defaults.master_profile", ""); p != "" {
		return p
	}
	return "orchestrator"
}
