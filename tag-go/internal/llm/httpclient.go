package llm

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// TAG must never hang silently. A provider (or a wedged proxy, or a local
// inference server that accepted the socket and then stalled) must fail fast
// with an honest error rather than block the CLI for minutes with zero output.
//
// The adapters used to fall back to a bare `&http.Client{Timeout: 10*time.Minute}`,
// whose only bound is on the WHOLE request — including the streamed body — so a
// server that accepts the connection and never writes a status line kept the
// process silent for the full ten minutes. The client below adds the bounds
// that matter for a streaming API:
//
//	DialContext.Timeout   — TCP connect
//	TLSHandshakeTimeout   — TLS negotiation
//	ResponseHeaderTimeout — time to the FIRST response byte (headers)
//
// ResponseHeaderTimeout is the one that catches the stalled-server case. It
// does NOT bound the SSE body, so a legitimately long completion still streams
// for as long as the overall Timeout allows.
const (
	// DefaultResponseHeaderTimeout bounds the wait for a provider's response
	// headers. Override with TAG_HTTP_HEADER_TIMEOUT (seconds; 0 disables).
	DefaultResponseHeaderTimeout = 60 * time.Second
	// DefaultRequestTimeout bounds a whole request including the streamed body.
	DefaultRequestTimeout = 10 * time.Minute
	// DefaultDialTimeout bounds the TCP connect.
	DefaultDialTimeout = 10 * time.Second
)

var (
	defaultClientOnce sync.Once
	defaultClient     *http.Client
)

// DefaultHTTPClient returns the shared, timeout-bounded client used by every
// provider adapter that wasn't given an explicit HTTPClient.
func DefaultHTTPClient() *http.Client {
	defaultClientOnce.Do(func() {
		defaultClient = &http.Client{
			Timeout: DefaultRequestTimeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: DefaultDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ResponseHeaderTimeout: envDuration("TAG_HTTP_HEADER_TIMEOUT", DefaultResponseHeaderTimeout),
				MaxIdleConns:          32,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	})
	return defaultClient
}

// envDuration reads an integer number of seconds from env, falling back to def.
// A value of 0 disables the bound; a malformed value falls back to def.
func envDuration(env string, def time.Duration) time.Duration {
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * time.Second
}
