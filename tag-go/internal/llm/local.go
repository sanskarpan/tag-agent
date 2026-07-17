package llm

import (
	"context"
	"net/http"
	"os"
)

// LocalProvider talks to a LOCAL OpenAI-compatible inference server — llama.cpp's
// llama-server, ollama's /v1 endpoint, LM Studio, or vLLM (gap #4 from the
// hermes-octo parity review). It requires no API key by default, which makes it
// the ideal last-resort step at the bottom of a route-fallback chain: when every
// cloud provider is rate-limited, unauthenticated, or down, a CPU-local model
// keeps the gateway responding. It reuses the OpenAI body-builder + SSE parser
// via streamOpenAICompatible, so tool-calling and usage accounting work too.
type LocalProvider struct {
	// BaseURL is the local server's OpenAI-compatible root (…/v1). Defaults to
	// TAG_LOCAL_BASE_URL, then llama.cpp's http://localhost:8080/v1.
	BaseURL string
	// APIKey is optional; most local servers ignore auth. Falls back to
	// TAG_LOCAL_API_KEY when set.
	APIKey     string
	HTTPClient *http.Client
}

// Name is the provider slug used in "local/<model>" refs and the registry.
func (LocalProvider) Name() string { return "local" }

func (p LocalProvider) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	if v := os.Getenv("TAG_LOCAL_BASE_URL"); v != "" {
		return v
	}
	return "http://localhost:8080/v1"
}

func (p LocalProvider) key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	return os.Getenv("TAG_LOCAL_API_KEY") // usually empty — local servers don't require auth
}

// Stream sends the request to the local server and decodes its SSE response.
func (p LocalProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	return streamOpenAICompatible(ctx, req, p.base(), p.key(), "local", p.HTTPClient)
}

func init() { Register(LocalProvider{}) }
