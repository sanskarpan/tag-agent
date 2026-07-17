package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/store"
)

func ghSig(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignatureGitHub(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	if !VerifySignature("github", body, ghSig("s3cr3t", body), "s3cr3t", "") {
		t.Error("valid github signature should verify")
	}
	if VerifySignature("github", body, ghSig("wrong", body), "s3cr3t", "") {
		t.Error("mismatched signature must fail")
	}
	if VerifySignature("github", body, "", "s3cr3t", "") {
		t.Error("missing sha256= prefix must fail")
	}
	if VerifySignature("github", body, ghSig("s3cr3t", body), "", "") {
		t.Error("no configured secret must return false")
	}
}

func slackSig(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + ts + ":"))
	m.Write(body)
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignatureSlackTimestampFreshness(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	secret := "s3cr3t"

	fresh := strconv.FormatInt(time.Now().Unix(), 10)
	if !VerifySignature("slack", body, slackSig(secret, fresh, body), secret, fresh) {
		t.Error("fresh slack timestamp should verify")
	}

	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	if VerifySignature("slack", body, slackSig(secret, stale, body), secret, stale) {
		t.Error("stale slack timestamp must be rejected")
	}

	future := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	if VerifySignature("slack", body, slackSig(secret, future, body), secret, future) {
		t.Error("far-future slack timestamp must be rejected")
	}

	if VerifySignature("slack", body, slackSig(secret, "nonsense", body), secret, "nonsense") {
		t.Error("non-numeric slack timestamp must be rejected")
	}
}

func TestParseEventGitHub(t *testing.T) {
	payload := map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"title": "Fix bug", "body": "details", "html_url": "http://x",
			"labels": []any{map[string]any{"name": "bug"}},
		},
	}
	info := ParseEvent("github", payload)
	if info.Type != "pull_request.opened" || info.Title != "Fix bug" || len(info.Labels) != 1 || info.Labels[0] != "bug" {
		t.Errorf("parse github wrong: %+v", info)
	}
}

func TestEventMatches(t *testing.T) {
	if !EventMatches("pull_request.*", "pull_request.opened") {
		t.Error("glob should match")
	}
	if EventMatches("issue", "issues.opened") {
		t.Error("must not do unanchored prefix match")
	}
	if EventMatches("", "anything") {
		t.Error("empty pattern matches nothing")
	}
}

func testDB(t *testing.T) *store.DB {
	db, err := store.OpenPath(t.TempDir() + "/w.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMatchRulesWithLabelFilter(t *testing.T) {
	db := testDB(t)
	CreateRule(db, "github", "pull_request.*", "coder", "run", []string{"bug"})
	CreateRule(db, "github", "issue.*", "reviewer", "run", nil)
	info := EventInfo{Type: "pull_request.opened", Labels: []string{"bug"}}
	matched, err := MatchRules(db, "github", "pull_request.opened", info)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0].Profile != "coder" {
		t.Errorf("expected the label-filtered PR rule, got %+v", matched)
	}
	// wrong label -> no match on the filtered rule
	info2 := EventInfo{Type: "pull_request.opened", Labels: []string{"docs"}}
	matched2, _ := MatchRules(db, "github", "pull_request.opened", info2)
	if len(matched2) != 0 {
		t.Errorf("label filter should exclude: %+v", matched2)
	}
}

func TestHandlerEnforcesSignatureAndEnqueues(t *testing.T) {
	db := testDB(t)
	CreateRule(db, "github", "pull_request.*", "coder", "run", nil)
	secret := "topsecret"
	srv := httptest.NewServer(Handler(db, secret, false))
	defer srv.Close()

	body := []byte(`{"action":"opened","pull_request":{"title":"T"}}`)

	// forged (no/invalid signature) -> 401, nothing enqueued
	resp, _ := http.Post(srv.URL+"/webhook/github", "application/json", strings.NewReader(string(body)))
	if resp.StatusCode != 401 {
		t.Errorf("unsigned request should be 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// valid signature -> 200, one queue job enqueued, event recorded
	req, _ := http.NewRequest("POST", srv.URL+"/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", ghSig(secret, body))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("signed request should be 200, got %d", resp2.StatusCode)
	}
	var out struct {
		RulesMatched   int  `json:"rules_matched"`
		SignatureValid bool `json:"signature_valid"`
	}
	json.NewDecoder(resp2.Body).Decode(&out)
	if out.RulesMatched != 1 || !out.SignatureValid {
		t.Errorf("expected 1 rule matched + valid sig: %+v", out)
	}
	var jobs int
	db.QueryRow(`SELECT COUNT(*) FROM queue_jobs WHERE profile='coder'`).Scan(&jobs)
	if jobs != 1 {
		t.Errorf("a queue job should be enqueued for the matched rule, got %d", jobs)
	}
	var events int
	db.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE signature_valid=1`).Scan(&events)
	if events != 1 {
		t.Errorf("the event should be recorded as signature_valid, got %d", events)
	}
}

func TestHandlerRejectsDuplicateDelivery(t *testing.T) {
	db := testDB(t)
	CreateRule(db, "github", "pull_request.*", "coder", "run", nil)
	secret := "topsecret"
	srv := httptest.NewServer(Handler(db, secret, false))
	defer srv.Close()

	body := []byte(`{"action":"opened","pull_request":{"title":"T"}}`)
	send := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/webhook/github", strings.NewReader(string(body)))
		req.Header.Set("X-Hub-Signature-256", ghSig(secret, body))
		req.Header.Set("X-GitHub-Delivery", "d-123")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send(); code != 200 {
		t.Fatalf("first delivery should be 200, got %d", code)
	}
	if code := send(); code != 409 {
		t.Errorf("replayed delivery should be 409, got %d", code)
	}
	var jobs int
	db.QueryRow(`SELECT COUNT(*) FROM queue_jobs`).Scan(&jobs)
	if jobs != 1 {
		t.Errorf("replay must not enqueue a second job, got %d", jobs)
	}
}

func TestRulesEndpointRequiresSecretWhenConfigured(t *testing.T) {
	db := testDB(t)
	CreateRule(db, "github", "pull_request.*", "coder", "run", nil)
	secret := "topsecret"
	srv := httptest.NewServer(Handler(db, secret, false))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/webhooks/rules")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("rules without token should be 401, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/webhooks/rules", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("rules with correct token should be 200, got %d", resp2.StatusCode)
	}
	var rules []Rule
	json.NewDecoder(resp2.Body).Decode(&rules)
	if len(rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(rules))
	}

	// secretless local dev keeps the endpoint open
	open := httptest.NewServer(Handler(db, "", true))
	defer open.Close()
	resp3, err := http.Get(open.URL + "/webhooks/rules")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Errorf("secretless rules endpoint should stay open, got %d", resp3.StatusCode)
	}
}

// When no secret is configured, the default (secure) handler must reject events
// so an anonymous caller cannot inject queue jobs; --allow-unsigned opts in.
func TestHandlerNoSecretDefaultsSecure(t *testing.T) {
	db := testDB(t)
	CreateRule(db, "github", "pull_request.*", "coder", "run", nil)
	body := []byte(`{"action":"opened","pull_request":{"title":"T"}}`)

	// default (allowUnsigned=false) -> 401, nothing enqueued
	secure := httptest.NewServer(Handler(db, "", false))
	resp, _ := http.Post(secure.URL+"/webhook/github", "application/json", strings.NewReader(string(body)))
	if resp.StatusCode != 401 {
		t.Errorf("no-secret default should reject with 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	secure.Close()
	var jobs int
	db.QueryRow(`SELECT COUNT(*) FROM queue_jobs`).Scan(&jobs)
	if jobs != 0 {
		t.Errorf("no job should be enqueued when rejected, got %d", jobs)
	}

	// explicit opt-in (allowUnsigned=true) -> 200, job enqueued
	open := httptest.NewServer(Handler(db, "", true))
	defer open.Close()
	resp2, _ := http.Post(open.URL+"/webhook/github", "application/json", strings.NewReader(string(body)))
	if resp2.StatusCode != 200 {
		t.Errorf("--allow-unsigned should accept with 200, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()
	db.QueryRow(`SELECT COUNT(*) FROM queue_jobs`).Scan(&jobs)
	if jobs != 1 {
		t.Errorf("allow-unsigned should enqueue the matched rule's job, got %d", jobs)
	}
}
