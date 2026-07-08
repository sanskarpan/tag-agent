package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
