package cli

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/gateway"
	"github.com/tag-agent/tag/internal/llm"
)

// registerGateway wires `tag gateway` — an OpenAI-compatible chat API in front
// of the native agent loop (gap #1). It exposes /v1/chat/completions (streaming
// + non-stream), /v1/models, and /health with optional bearer auth, and reuses
// the runtime fallback chain (gap #2) so a request fails over across providers.
func registerGateway(root *cobra.Command, app *App) {
	var host, key, defProvider, profile string
	var port int
	var useFallback, allowUnauth bool

	c := &cobra.Command{
		Use:     "gateway",
		Short:   "Serve the agent as an OpenAI-compatible /v1 chat API",
		GroupID: "orch",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			k := key
			if k == "" {
				k = os.Getenv("TAG_GATEWAY_KEY")
			}
			bindHost := strOr(host, "127.0.0.1")
			// Security: a non-loopback bind must be authenticated. Without a key,
			// only loopback is allowed (or an explicit --allow-unauthenticated
			// opt-in, which is loud and INSECURE) — mirrors the webhook hardening.
			if k == "" && !allowUnauth && !isLoopbackHost(bindHost) {
				return fmt.Errorf("refusing to bind %s without an auth key: set --key / TAG_GATEWAY_KEY, bind 127.0.0.1, or pass --allow-unauthenticated", bindHost)
			}

			defProv, ok := llm.Registry[defProvider]
			if !ok {
				return fmt.Errorf("unknown default provider %q (available: %v)", defProvider, providerNames())
			}

			resolve := func(model string) (llm.Provider, string, error) {
				return gatewayResolve(app, defProv, defProvider, profile, useFallback, model)
			}

			opts := gateway.Options{
				Key:                  k,
				AllowUnauthenticated: allowUnauth || (k == "" && isLoopbackHost(bindHost)),
				Resolve:              resolve,
				DefaultModel:         app.Cfg.String("profiles."+app.profile(profile)+".config.model.default", ""),
				Models:               gatewayModels(app),
				MaxTokens:            4096,
			}

			addr := fmt.Sprintf("%s:%d", bindHost, port)
			fmt.Printf("TAG gateway: http://%s/v1  (OpenAI-compatible; Ctrl+C to stop)\n", addr)
			if k == "" {
				fmt.Println("WARNING: no auth key set — accepting UNAUTHENTICATED requests (loopback only unless --allow-unauthenticated).")
			}
			return (&http.Server{Addr: addr, Handler: gateway.Handler(opts)}).ListenAndServe()
		},
	}
	c.Flags().StringVar(&host, "host", "127.0.0.1", "bind host")
	c.Flags().IntVar(&port, "port", 8787, "bind port")
	c.Flags().StringVar(&key, "key", "", "bearer token required from clients (or TAG_GATEWAY_KEY)")
	c.Flags().StringVar(&defProvider, "provider", "echo", "default provider when a request model has no provider/ prefix")
	c.Flags().StringVar(&profile, "profile", "", "profile (for default model + fallback chain)")
	c.Flags().BoolVar(&useFallback, "fallback", false, "walk the profile's route-fallback chain on a retryable provider error")
	c.Flags().BoolVar(&allowUnauth, "allow-unauthenticated", false, "accept unauthenticated requests on a non-loopback bind (INSECURE)")
	root.AddCommand(c)
}

// gatewayResolve maps a requested model to a provider + bare model id. A model
// with a "provider/" prefix selects that provider; otherwise the default
// provider is used. When --fallback is set and a route_fallbacks chain exists
// for the model, the provider is wrapped so it fails over at inference time.
func gatewayResolve(app *App, defProv llm.Provider, defProvSlug, profile string, useFallback bool, model string) (llm.Provider, string, error) {
	prov := defProv
	provSlug := defProvSlug
	if i := strings.IndexByte(model, '/'); i > 0 {
		provSlug = model[:i]
		p := llm.Registry[provSlug]
		if p == nil {
			return nil, "", fmt.Errorf("no registered provider %q for model %q", provSlug, model)
		}
		prov = p
	}
	bare := stripProviderPrefix(model)
	if useFallback {
		if fp, err := buildFallbackProvider(app, prov, provSlug, bare, profile); err == nil && fp != nil {
			return fp, bare, nil
		}
	}
	return prov, bare, nil
}

// gatewayModels advertises the distinct configured primary models across all
// profiles for GET /v1/models, plus the offline echo model.
func gatewayModels(app *App) []string {
	set := map[string]bool{"echo": true}
	if profs := app.Cfg.Section("profiles"); profs != nil {
		for name := range profs {
			m := app.Cfg.String("profiles."+name+".config.model.default", "")
			p := app.Cfg.String("profiles."+name+".config.model.provider", "")
			if m == "" {
				continue
			}
			if p != "" && !strings.Contains(m, "/") {
				m = p + "/" + m
			}
			set[m] = true
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func isLoopbackHost(h string) bool {
	h = strings.TrimSpace(h)
	return h == "" || h == "127.0.0.1" || h == "localhost" || h == "::1" || h == "[::1]"
}
