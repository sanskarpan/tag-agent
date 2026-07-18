// Package marketplace holds the reusable logic for the `tag marketplace`
// command: the SSRF fetch guard (ValidateFetchURL) and a small fetch
// abstraction. Ported from src/tag/cmd/marketplace.py (cmd_profile_marketplace).
package marketplace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// ValidateFetchURL restricts outbound fetches to public http/https hosts
// (SSRF / file:// guard). It rejects:
//   - non-http(s) schemes (notably file://)
//   - an empty host
//   - hosts that are, or resolve to, loopback (127.0.0.0/8, ::1),
//     link-local (169.254.0.0/16, fe80::/10), private ranges
//     (10/8, 172.16/12, 192.168/16), or the cloud metadata IP
//     169.254.169.254.
//
// Hostnames are resolved best-effort: if DNS resolution fails the connection
// would fail anyway, so we do not block on that. THIS FUNCTION DOES NO GET.
func ValidateFetchURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		scheme := parsed.Scheme
		if scheme == "" {
			scheme = "(none)"
		}
		return fmt.Errorf("unsupported URL scheme %q: only http/https are allowed", scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}

	var candidates []net.IP
	if ip := net.ParseIP(host); ip != nil {
		candidates = []net.IP{ip}
	} else {
		// Hostname — resolve best-effort; ignore resolution failures.
		if addrs, err := net.LookupIP(host); err == nil {
			candidates = addrs
		}
	}

	for _, ip := range candidates {
		if isBlockedIP(ip) {
			return fmt.Errorf("refusing to fetch from non-public address %s (host %q)", ip, host)
		}
	}
	return nil
}

// allowLoopbackForTest, when true, exempts loopback addresses from the SSRF
// guard so tests can point Fetch/PushJSON at an httptest server (which always
// binds 127.0.0.1). It defaults to false and is ONLY ever enabled from tests:
// in-process unit tests set it directly, and the subprocess CLI E2E flips it
// via TAG_MARKETPLACE_ALLOW_LOOPBACK=1 — but that env hook is compiled in ONLY
// under the `ssrf_testhook` build tag (see marketplace_loopback_testhook.go).
// Production builds omit that tag, so the shipped binary has no runtime switch
// to disable the loopback guard. Only loopback is exempted; private/link-local/
// reserved stay blocked even when it is on.
var allowLoopbackForTest = false

// isBlockedIP reports whether ip is a non-public (SSRF-sensitive) address.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if allowLoopbackForTest && ip.IsLoopback() {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	// Explicit cloud metadata address (covered by link-local above, kept
	// explicit to mirror the Python guard).
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return true
	}
	// Reserved / special-use ranges the stdlib helpers don't all cover:
	// CGNAT 100.64/10, TEST-NET 192.0.2/24, benchmarking 198.18/15,
	// 240/4 reserved, IPv4 broadcast.
	for _, cidr := range reservedCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var reservedCIDRs = func() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{
		"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
		"100::/64", "2001:db8::/32",
	} {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

const maxFetchBytes = 8 * 1024 * 1024

// Fetch performs the network GET. It is only called from the live command path
// (never from tests). The URL MUST already have passed ValidateFetchURL, but
// Fetch ALSO defends in depth: a socket-level Control hook rejects any IP the
// connection actually dials — closing the redirect-to-internal and DNS-rebinding
// (TOCTOU) holes that a pre-flight URL check alone can't — and every redirect
// hop is re-validated. The body is size-capped.
func Fetch(rawURL string, timeout time.Duration) ([]byte, error) {
	client := guardedClient(timeout)
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch failed: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFetchBytes {
		return nil, fmt.Errorf("fetch failed: response body exceeds %d bytes", maxFetchBytes)
	}
	return b, nil
}

// guardedClient builds the SSRF-hardened *http.Client shared by Fetch (GET) and
// PushJSON (POST). Its socket-level Control hook rejects any IP the connection
// actually dials — closing redirect-to-internal and DNS-rebinding (TOCTOU) holes
// a pre-flight URL check alone can't — and every redirect hop is re-validated.
func guardedClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
				return fmt.Errorf("refusing to connect to non-public address %s", ip)
			}
			return nil
		},
	}
	transport := &http.Transport{DialContext: dialer.DialContext}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return ValidateFetchURL(req.URL.String())
		},
	}
}

// PushResult is the outcome of a PushJSON call: the server's HTTP status and its
// (size-capped) response body, so the caller can report what the endpoint said.
type PushResult struct {
	StatusCode int
	Status     string
	Body       []byte
}

// PushJSON POSTs body (as application/json) to rawURL under the SAME SSRF
// protections as Fetch: block loopback/internal/reserved IPs at the socket level
// and re-validate on every redirect. The URL MUST already have passed
// ValidateFetchURL (the caller pre-flights it, mirroring pull); PushJSON also
// defends in depth via the shared guarded client. The response body is
// size-capped. A non-2xx status is returned as an error but still carries the
// PushResult so the caller can surface the server's message.
func PushJSON(rawURL string, body []byte, timeout time.Duration) (*PushResult, error) {
	client := guardedClient(timeout)
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFetchBytes {
		return nil, fmt.Errorf("push failed: response body exceeds %d bytes", maxFetchBytes)
	}
	res := &PushResult{StatusCode: resp.StatusCode, Status: resp.Status, Body: b}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return res, fmt.Errorf("push failed: HTTP %d", resp.StatusCode)
	}
	return res, nil
}

// SHA256Hex returns the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
