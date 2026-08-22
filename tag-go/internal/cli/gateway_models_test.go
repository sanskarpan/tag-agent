package cli

import (
	"testing"

	"github.com/tag-agent/tag/internal/config"
)

// TestGatewayModelsOnlyAdvertisesServable: GET /v1/models must not advertise a
// model whose provider is not registered — the gateway would then reject it at
// /v1/chat/completions with "no registered provider", breaking an OpenAI client
// that trusts the model list (#740).
func TestGatewayModelsOnlyAdvertisesServable(t *testing.T) {
	app := &App{Cfg: &config.Config{Data: map[string]any{
		"profiles": map[string]any{
			// openai IS a registered provider -> servable.
			"good": map[string]any{"config": map[string]any{
				"model": map[string]any{"default": "gpt-4o", "provider": "openai"}}},
			// deepseek is NOT registered -> must be dropped.
			"bad": map[string]any{"config": map[string]any{
				"model": map[string]any{"default": "deepseek-v4", "provider": "deepseek"}}},
		},
	}}}

	got := map[string]bool{}
	for _, m := range gatewayModels(app) {
		got[m] = true
	}
	if !got["echo"] {
		t.Error("echo must always be advertised")
	}
	if !got["openai/gpt-4o"] {
		t.Error("a model with a registered provider must be advertised")
	}
	if got["deepseek/deepseek-v4"] {
		t.Error("a model with an UNregistered provider must not be advertised (it cannot be served)")
	}
}
