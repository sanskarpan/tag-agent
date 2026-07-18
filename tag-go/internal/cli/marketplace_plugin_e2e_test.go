package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// runEnv is like run() but with extra env vars (KEY=VALUE) appended. Used to
// flip TAG_MARKETPLACE_ALLOW_LOOPBACK for the mock-server E2E, which the
// production SSRF guard would otherwise refuse (httptest binds 127.0.0.1).
func runEnv(t *testing.T, home string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestE2EMarketplacePushRoundTrip drives the real binary: pull a profile from a
// mock marketplace, then push it to another mock endpoint. Asserts the push
// server actually received the profile config JSON and the CLI reports success.
func TestE2EMarketplacePushRoundTrip(t *testing.T) {
	h := newHome(t)
	allowLoopback := []string{"TAG_MARKETPLACE_ALLOW_LOOPBACK=1"}

	const profileYAML = "name: shipped\nmodel: gpt-4o\ntemperature: 0.2\n"

	// Source marketplace: serves a profile YAML for `pull` to fetch.
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write([]byte(profileYAML))
	}))
	defer source.Close()

	// Push target: records what the CLI POSTs.
	var (
		mu         sync.Mutex
		gotMethod  string
		gotCT      string
		gotPayload map[string]any
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotPayload)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"accepted","id":"gist-123"}`))
	}))
	defer target.Close()

	// pull -> seeds runtime config.yaml for profile "shipped".
	if out, code := runEnv(t, h, allowLoopback, "marketplace", "pull", source.URL, "--name", "shipped"); code != 0 || !strings.Contains(out, "Pulled profile: shipped") {
		t.Fatalf("pull failed: %q code=%d", out, code)
	}

	// push -> POSTs that config to the target marketplace.
	out, code := runEnv(t, h, allowLoopback, "marketplace", "push", "shipped", "--url", target.URL)
	if code != 0 {
		t.Fatalf("push failed: %q code=%d", out, code)
	}
	if !strings.Contains(out, "Pushed profile: shipped") {
		t.Errorf("push output missing success line: %q", out)
	}
	if !strings.Contains(out, "201") {
		t.Errorf("push output should report the 201 status: %q", out)
	}
	if !strings.Contains(out, "gist-123") {
		t.Errorf("push output should echo the server response: %q", out)
	}

	// Assert the target server actually received the profile config JSON.
	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("server saw method %q, want POST", gotMethod)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("server saw content-type %q, want application/json", gotCT)
	}
	if gotPayload["name"] != "shipped" {
		t.Errorf("payload name = %v, want shipped", gotPayload["name"])
	}
	cfg, _ := gotPayload["config"].(map[string]any)
	if cfg == nil {
		t.Fatalf("payload missing config object: %+v", gotPayload)
	}
	if cfg["model"] != "gpt-4o" || cfg["name"] != "shipped" {
		t.Errorf("pushed config did not match the profile: %+v", cfg)
	}
}

// TestE2EMarketplacePushJSONFlag checks the --json machine-readable output.
func TestE2EMarketplacePushJSONFlag(t *testing.T) {
	h := newHome(t)
	allowLoopback := []string{"TAG_MARKETPLACE_ALLOW_LOOPBACK=1"}

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("name: jprofile\nk: v\n"))
	}))
	defer source.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()

	if _, code := runEnv(t, h, allowLoopback, "marketplace", "pull", source.URL, "--name", "jprofile"); code != 0 {
		t.Fatalf("pull failed code=%d", code)
	}
	out, code := runEnv(t, h, allowLoopback, "--json", "marketplace", "push", "jprofile", "--url", target.URL)
	if code != 0 {
		t.Fatalf("push --json failed: %q code=%d", out, code)
	}
	// Isolate the JSON object (pull/other lines may precede it).
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in output: %q", out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out[start:]), &res); err != nil {
		t.Fatalf("output not valid JSON: %v\n%q", err, out)
	}
	if res["name"] != "jprofile" {
		t.Errorf("json name = %v", res["name"])
	}
	if sc, _ := res["status_code"].(float64); sc != 200 {
		t.Errorf("json status_code = %v, want 200", res["status_code"])
	}
}

// TestE2EMarketplacePushSSRFRefused verifies the SSRF guard: pushing to a
// loopback/internal URL is REFUSED (guard OFF, i.e. production behavior).
func TestE2EMarketplacePushSSRFRefused(t *testing.T) {
	h := newHome(t)
	allowLoopback := []string{"TAG_MARKETPLACE_ALLOW_LOOPBACK=1"}

	// Seed a profile via a public-looking source (loopback allowed for the pull).
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("name: victim\n"))
	}))
	defer source.Close()
	if _, code := runEnv(t, h, allowLoopback, "marketplace", "pull", source.URL, "--name", "victim"); code != 0 {
		t.Fatalf("seed pull failed code=%d", code)
	}

	// Now push WITHOUT the loopback exemption (default = production guard on).
	// Every one of these internal targets must be refused.
	for _, bad := range []string{
		"http://127.0.0.1:9/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/ingest",
		"file:///etc/passwd",
	} {
		out, code := run(t, h, "marketplace", "push", "victim", "--url", bad)
		if code == 0 {
			t.Errorf("push to %s must be refused, but succeeded: %q", bad, out)
		}
		if !strings.Contains(out, "refused") && !strings.Contains(out, "non-public") && !strings.Contains(out, "scheme") {
			t.Errorf("push to %s: expected SSRF/scheme refusal, got: %q", bad, out)
		}
	}
}

// TestE2EMarketplacePushMissingProfile: pushing an unknown profile errors.
func TestE2EMarketplacePushMissingProfile(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "marketplace", "push", "nope", "--url", "https://example.com/ingest")
	if code == 0 {
		t.Errorf("push of a missing profile must fail: %q", out)
	}
	if !strings.Contains(out, "no config") {
		t.Errorf("expected a missing-config error, got: %q", out)
	}
}

// TestE2EMarketplacePushRequiresURL: --url is mandatory.
func TestE2EMarketplacePushRequiresURL(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "marketplace", "push", "x"); code == 0 || !strings.Contains(out, "--url") {
		t.Errorf("push without --url must fail naming --url: %q code=%d", out, code)
	}
}

// ---- plugin install E2E ----

// TestE2EPluginInstallRecordsAndEnables: installing a curated plugin records +
// enables it; `plugin list` marks it and the .env carries the enable flag.
func TestE2EPluginInstallRecordsAndEnables(t *testing.T) {
	h := newHome(t)
	const plugin = "hermes-code-tools" // curated, requires_env: []

	out, code := run(t, h, "plugin", "install", plugin)
	if code != 0 {
		t.Fatalf("install failed: %q code=%d", out, code)
	}
	if !strings.Contains(out, "Installed plugin '"+plugin+"'") {
		t.Errorf("install output missing confirmation: %q", out)
	}
	if !strings.Contains(out, "Enabled plugin '"+plugin+"'") {
		t.Errorf("install should also enable the plugin: %q", out)
	}

	// plugin list --json marks it installed.
	lst, code := run(t, h, "plugin", "list", "--json")
	if code != 0 {
		t.Fatalf("plugin list failed code=%d", code)
	}
	var rows []map[string]any
	start := strings.Index(lst, "[")
	if start < 0 {
		t.Fatalf("no JSON array in list: %q", lst)
	}
	if err := json.Unmarshal([]byte(lst[start:]), &rows); err != nil {
		t.Fatalf("list not JSON: %v\n%q", err, lst)
	}
	found := false
	for _, r := range rows {
		if r["name"] == plugin {
			found = true
			if r["installed"] != true {
				t.Errorf("plugin %s should be marked installed: %+v", plugin, r)
			}
		}
	}
	if !found {
		t.Errorf("plugin %s missing from list", plugin)
	}
}

// TestE2EPluginInstallUnknownErrors: unknown plugin errors, records nothing.
func TestE2EPluginInstallUnknownErrors(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "plugin", "install", "totally-made-up-plugin")
	if code == 0 {
		t.Errorf("unknown plugin install must fail: %q", out)
	}
	if !strings.Contains(out, "unknown plugin") {
		t.Errorf("expected 'unknown plugin' error, got: %q", out)
	}
}

// TestE2EPluginInstallMissingEnvNoFakeSuccess: a plugin declaring requires_env
// that is NOT set must fail honestly and record NOTHING (no fake success).
func TestE2EPluginInstallMissingEnvNoFakeSuccess(t *testing.T) {
	h := newHome(t)
	const plugin = "hermes-web-search" // requires_env: [SERP_API_KEY]

	// Strip SERP_API_KEY entirely from the child env (an empty value still counts
	// as "set" via LookupEnv), so the honesty guard must trip.
	cmd := exec.Command(tagBin, "plugin", "install", plugin)
	env := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SERP_API_KEY=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "TAG_HOME="+h)
	cmd.Env = env
	raw, err := cmd.CombinedOutput()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	}
	got := string(raw)
	if exit == 0 {
		t.Fatalf("install must fail when SERP_API_KEY is unset: %q", got)
	}
	if !strings.Contains(got, "SERP_API_KEY") {
		t.Errorf("error must name the missing env var: %q", got)
	}
	if strings.Contains(got, "Installed plugin") {
		t.Errorf("must NOT report fake success: %q", got)
	}

	// Verify NOTHING was recorded: plugin list must not mark it installed.
	lst, _ := run(t, h, "plugin", "list", "--json")
	var rows []map[string]any
	if s := strings.Index(lst, "["); s >= 0 {
		_ = json.Unmarshal([]byte(lst[s:]), &rows)
	}
	for _, r := range rows {
		if r["name"] == plugin && r["installed"] == true {
			t.Errorf("failed install must not be recorded, but %s is marked installed", plugin)
		}
	}
}

// TestE2EPluginInstallWithEnvSucceeds: with the required env present, the
// requires_env plugin installs + enables.
func TestE2EPluginInstallWithEnvSucceeds(t *testing.T) {
	h := newHome(t)
	const plugin = "hermes-web-search"
	out, code := runEnv(t, h, []string{"SERP_API_KEY=dummy-key"}, "plugin", "install", plugin)
	if code != 0 {
		t.Fatalf("install with env set should succeed: %q code=%d", out, code)
	}
	if !strings.Contains(out, "Installed plugin '"+plugin+"'") {
		t.Errorf("expected install confirmation: %q", out)
	}
}
