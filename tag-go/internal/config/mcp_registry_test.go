package config

import "testing"

func TestMCPRegistryEmbedded(t *testing.T) {
	servers, err := MCPRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) < 5 {
		t.Errorf("expected the bundled catalog to have several servers, got %d", len(servers))
	}
	gh, ok := servers["mcp-github"].(map[string]any)
	if !ok {
		t.Fatal("mcp-github should be in the catalog")
	}
	if gh["category"] != "vcs" {
		t.Errorf("mcp-github category should be vcs, got %v", gh["category"])
	}
}

func TestPluginRegistryEmbedded(t *testing.T) {
	reg, err := PluginRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg) < 3 {
		t.Errorf("expected several bundled plugins, got %d", len(reg))
	}
	if _, ok := reg["hermes-web-search"]; !ok {
		t.Error("hermes-web-search should be in the plugin registry")
	}
}
