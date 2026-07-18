//go:build ssrf_testhook

package marketplace

import "os"

// This test-only hook is compiled ONLY into binaries built with the
// `ssrf_testhook` build tag (the subprocess CLI E2E). It lets those tests point
// the SSRF-guarded client at an httptest loopback server via
// TAG_MARKETPLACE_ALLOW_LOOPBACK=1. Production builds omit this tag, so the env
// var is never consulted and the loopback guard cannot be disabled at runtime.
func init() {
	if os.Getenv("TAG_MARKETPLACE_ALLOW_LOOPBACK") == "1" {
		allowLoopbackForTest = true
	}
}
