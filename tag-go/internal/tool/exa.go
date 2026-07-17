package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// exaSearchTool exposes Exa (exa.ai) web search + content extraction to the agent
// as a `web_search` tool (gap #5 from the hermes-octo parity review, which uses
// Exa as its web backend). It is OFF by default — tool-budget discipline keeps
// the model's tool list lean, and it needs an API key — and is enabled via
// Options.EnableExa + EXA_API_KEY. The BaseURL is overridable for offline tests.
func exaSearchTool(opts Options) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{
			Name:        "web_search",
			Description: "Search the web (via Exa) and return titles, URLs, and short extracts for a query. Use for current events or facts not in the prompt.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":       map[string]any{"type": "string", "description": "the search query"},
					"num_results": map[string]any{"type": "integer", "description": "how many results (default 5, max 10)"},
				},
				"required": []string{"query"},
			},
		},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			query := strings.TrimSpace(strArg(in, "query"))
			if query == "" {
				return "", fmt.Errorf("query is required")
			}
			key := opts.exaKey()
			if key == "" {
				return "", fmt.Errorf("web_search unavailable: EXA_API_KEY is not set")
			}
			n := 5
			if v, ok := in["num_results"].(float64); ok && v > 0 {
				n = int(v)
			}
			if n > 10 {
				n = 10
			}
			return exaSearch(ctx, opts, key, query, n)
		},
	}
}

func exaSearch(ctx context.Context, opts Options, key, query string, n int) (string, error) {
	base := opts.ExaBaseURL
	if base == "" {
		base = os.Getenv("EXA_BASE_URL")
	}
	if base == "" {
		base = "https://api.exa.ai"
	}
	body, _ := json.Marshal(map[string]any{
		"query":      query,
		"numResults": n,
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": 800}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/search", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", key)
	client := opts.ExaClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("exa API %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("exa: bad response: %w", err)
	}
	if len(out.Results) == 0 {
		return "No results.", nil
	}
	var sb strings.Builder
	for i, r := range out.Results {
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
		if t := strings.TrimSpace(r.Text); t != "" {
			fmt.Fprintf(&sb, "   %s\n", truncateOneLine(t, 400))
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func truncateOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
