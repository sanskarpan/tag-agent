package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/store"
)

// post sends body to the receiver with the supplied headers and returns the
// status code.
func post(t *testing.T, url, platform, body string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/webhook/"+platform, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func rawHMAC(secret string, msg []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

// TestReplayProtectionSurvivesRestart is the durability regression: the old
// implementation kept seen delivery IDs in a process-local map, so restarting
// the receiver reopened the replay window on every captured payload.
func TestReplayProtectionSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/w.sqlite3"
	secret := "topsecret"
	body := `{"action":"opened","pull_request":{"title":"T"}}`
	hdr := map[string]string{
		"X-Hub-Signature-256": ghSig(secret, []byte(body)),
		"X-GitHub-Delivery":   "d-restart",
	}

	run := func() (int, *store.DB) {
		db, err := store.OpenPath(path)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(Handler(db, secret, false))
		defer srv.Close()
		return post(t, srv.URL, "github", body, hdr), db
	}

	code, db1 := run()
	if code != 200 {
		t.Fatalf("first delivery should be 200, got %d", code)
	}
	db1.Close() // the process goes away; only the DB file survives

	code2, db2 := run()
	defer db2.Close()
	if code2 != 409 {
		t.Errorf("a replay after restart must still be 409, got %d", code2)
	}
	var events int
	db2.QueryRow(`SELECT COUNT(*) FROM webhook_events`).Scan(&events)
	if events != 1 {
		t.Errorf("the replay must not be recorded as a second event, got %d", events)
	}
}

// TestReplayProtectionSurvivesCacheEviction is the eviction regression: the old
// set was bounded at 4096 entries, so an attacker holding a captured payload
// only had to push it out with other traffic before replaying it.
func TestReplayProtectionSurvivesCacheEviction(t *testing.T) {
	db := testDB(t)
	if err := EnsureReplaySchema(db); err != nil {
		t.Fatal(err)
	}
	first := "github:id:d-0"
	if !markDelivered(db, "github", first) {
		t.Fatal("the first delivery should be new")
	}
	for i := 1; i <= maxSeenDeliveryIDsRegression; i++ {
		markDelivered(db, "github", "github:id:d-"+strconv.Itoa(i))
	}
	if markDelivered(db, "github", first) {
		t.Errorf("the original delivery must still be remembered after %d others", maxSeenDeliveryIDsRegression)
	}
}

// maxSeenDeliveryIDsRegression is the bound the removed in-memory cache used.
// Exceeding it is exactly what used to reopen the replay window.
const maxSeenDeliveryIDsRegression = 4200

// TestGenericSignedWebhookReplayRejected covers senders that carry no delivery
// id at all. They previously skipped replay protection entirely, so a captured
// signed body could be replayed forever.
func TestGenericSignedWebhookReplayRejected(t *testing.T) {
	db := testDB(t)
	secret := "topsecret"
	srv := httptest.NewServer(Handler(db, secret, false))
	defer srv.Close()

	body := `{"type":"deploy","title":"ship it"}`
	hdr := map[string]string{"X-Hub-Signature-256": rawHMAC(secret, []byte(body))}

	if code := post(t, srv.URL, "generic", body, hdr); code != 200 {
		t.Fatalf("first generic delivery should be 200, got %d", code)
	}
	if code := post(t, srv.URL, "generic", body, hdr); code != 409 {
		t.Errorf("a replayed generic delivery must be 409, got %d", code)
	}
}

// TestSlackReplayWithinToleranceRejected covers the ±5m signing window. The
// timestamp check alone leaves a five-minute window in which a captured payload
// verifies again; the signature fingerprint closes it.
func TestSlackReplayWithinToleranceRejected(t *testing.T) {
	db := testDB(t)
	secret := "topsecret"
	srv := httptest.NewServer(Handler(db, secret, false))
	defer srv.Close()

	body := `{"type":"event_callback","text":"deploy prod"}`
	// A timestamp inside the tolerance, as a captured-and-immediately-replayed
	// payload would be.
	ts := strconv.FormatInt(time.Now().Add(-30*time.Second).Unix(), 10)
	hdr := map[string]string{
		"X-Slack-Signature":         slackSig(secret, ts, []byte(body)),
		"X-Slack-Request-Timestamp": ts,
	}

	if code := post(t, srv.URL, "slack", body, hdr); code != 200 {
		t.Fatalf("first slack delivery should be 200, got %d", code)
	}
	if code := post(t, srv.URL, "slack", body, hdr); code != 409 {
		t.Errorf("a slack payload replayed inside the tolerance must be 409, got %d", code)
	}
}

// TestDistinctDeliveriesStillAccepted guards against the fingerprint being so
// coarse that legitimate traffic is dropped — the failure mode a fail-closed
// replay check invites.
func TestDistinctDeliveriesStillAccepted(t *testing.T) {
	db := testDB(t)
	secret := "topsecret"
	srv := httptest.NewServer(Handler(db, secret, false))
	defer srv.Close()

	for _, title := range []string{"one", "two", "three"} {
		body := `{"type":"deploy","title":"` + title + `"}`
		hdr := map[string]string{"X-Hub-Signature-256": rawHMAC(secret, []byte(body))}
		if code := post(t, srv.URL, "generic", body, hdr); code != 200 {
			t.Errorf("distinct payload %q should be accepted, got %d", title, code)
		}
	}
	// Same body, different delivery ids: the id is authoritative, so both pass.
	body := `{"action":"opened","pull_request":{"title":"T"}}`
	for _, id := range []string{"d-a", "d-b"} {
		hdr := map[string]string{
			"X-Hub-Signature-256": ghSig(secret, []byte(body)),
			"X-GitHub-Delivery":   id,
		}
		if code := post(t, srv.URL, "github", body, hdr); code != 200 {
			t.Errorf("delivery %s should be accepted, got %d", id, code)
		}
	}
}

// TestMarkDeliveredFailsClosed: an event whose replay status cannot be
// determined must be treated as a replay, not waved through.
func TestMarkDeliveredFailsClosed(t *testing.T) {
	if markDelivered(nil, "github", "github:id:x") {
		t.Error("markDelivered must fail closed when the DB is unavailable")
	}
	db := testDB(t)
	// Schema missing entirely -> Exec errors -> fail closed.
	db.Exec(`DROP TABLE IF EXISTS webhook_deliveries`)
	if markDelivered(db, "github", "github:id:x") {
		t.Error("markDelivered must fail closed when the insert fails")
	}
}

func TestDeliveryFingerprintIdentity(t *testing.T) {
	// NOTE: an earlier version of this test asserted "fingerprints must be
	// namespaced per platform". That property WAS the vulnerability — the
	// platform is the caller-controlled path segment, so namespacing by it let
	// one captured github delivery replay as /webhook/linear. The test encoded
	// the bug as a requirement and passed. It now asserts the opposite.
	if fp := deliveryFingerprint("github", "d-1", "sig"); fp != "id:d-1" {
		t.Errorf("an explicit delivery id is authoritative and platform-independent, got %q", fp)
	}
	if deliveryFingerprint("github", "d-1", "x") != deliveryFingerprint("linear", "d-1", "y") {
		t.Error("the same delivery id must collide across platforms — that is the point")
	}
	a := deliveryFingerprint("slack", "", "v0=abc")
	b := deliveryFingerprint("slack", "", "v0=abc")
	if a == "" || a != b {
		t.Errorf("the signature fingerprint must be stable, got %q / %q", a, b)
	}
	if deliveryFingerprint("linear", "", "v0=abc") != a {
		t.Error("the same signature must collide across platforms — a github signature " +
			"replayed at /webhook/linear has to be recognised as the same delivery")
	}
	if strings.Contains(a, "v0=abc") {
		t.Error("the raw signature must not be stored in the fingerprint")
	}
	if deliveryFingerprint("generic", "", "") != "" {
		t.Error("with no id and no signature there is nothing to key on")
	}
}

// TestUnknownPlatformIsRejected is the ISSUE-007 regression: the platform was an
// open set, so any string was accepted and got its own replay namespace.
func TestUnknownPlatformIsRejected(t *testing.T) {
	db := testDB(t)
	secret := "topsecret"
	srv := httptest.NewServer(Handler(db, secret, false))
	defer srv.Close()

	body := `{"action":"opened","pull_request":{"title":"T"}}`
	hdr := map[string]string{
		"X-Hub-Signature-256": ghSig(secret, []byte(body)),
		"X-GitHub-Delivery":   "d-confusion",
	}

	if code := post(t, srv.URL, "github", body, hdr); code != 200 {
		t.Fatalf("first github delivery should be 200, got %d", code)
	}
	// The same captured delivery, replayed by changing only the path segment.
	//
	// linear and generic share GitHub's bare-HMAC-of-body scheme, so the
	// signature still verifies and the replay check is what must catch it.
	for _, p := range []string{"linear", "generic"} {
		if code := post(t, srv.URL, p, body, hdr); code != 409 {
			t.Errorf("a github delivery replayed at /webhook/%s must be a duplicate (409), got %d", p, code)
		}
	}
	// slack signs "v0:<ts>:<body>" and requires a timestamp header, so a github
	// signature fails verification outright — a stronger rejection than 409,
	// and it must stay that way.
	if code := post(t, srv.URL, "slack", body, hdr); code != 401 {
		t.Errorf("a github signature at /webhook/slack must fail verification (401), got %d", code)
	}
	for _, p := range []string{"attacker", "zzz", "GitHubb", "a/b"} {
		if code := post(t, srv.URL, p, body, hdr); code != 404 {
			t.Errorf("unknown platform %q must be 404, got %d", p, code)
		}
	}
}

func TestKnownPlatformSet(t *testing.T) {
	for _, p := range []string{"github", "slack", "linear", "generic"} {
		if !KnownPlatform(p) {
			t.Errorf("%s must be accepted", p)
		}
	}
	for _, p := range []string{"", "attacker", "GITHUB", "github "} {
		if KnownPlatform(p) {
			t.Errorf("%q must not be accepted", p)
		}
	}
	if len(KnownPlatforms()) != 4 {
		t.Errorf("KnownPlatforms drifted: %v", KnownPlatforms())
	}
}
