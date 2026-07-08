// Package webhook is the CI/CD webhook receiver (Track B — PRD-056). It verifies
// HMAC signatures (GitHub/Slack/Linear/generic), parses platform events, matches
// trigger rules, and enqueues TAG queue jobs. The core logic is pure and the
// HTTP handler is a func of *store.DB, so all of it is testable offline.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tag-agent/tag/internal/store"
)

const maxBodyBytes = 10 * 1024 * 1024

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// VerifySignature validates an inbound webhook's HMAC-SHA256 signature. Returns
// false when no secret is configured (callers decide whether to enforce).
func VerifySignature(platform string, body []byte, sigHeader, secret, timestamp string) bool {
	if secret == "" {
		return false
	}
	sb := []byte(secret)
	switch platform {
	case "github":
		if !strings.HasPrefix(sigHeader, "sha256=") {
			return false
		}
		return hmacEqual(sb, body, strings.TrimPrefix(sigHeader, "sha256="))
	case "slack":
		if !strings.HasPrefix(sigHeader, "v0=") || timestamp == "" {
			return false
		}
		base := append([]byte("v0:"+timestamp+":"), body...)
		return hmacEqual(sb, base, strings.TrimPrefix(sigHeader, "v0="))
	default: // linear / generic
		sig := sigHeader
		if i := strings.LastIndex(sigHeader, "="); i >= 0 {
			sig = sigHeader[i+1:]
		}
		return hmacEqual(sb, body, sig)
	}
}

func hmacEqual(secret, msg []byte, sigHex string) bool {
	m := hmac.New(sha256.New, secret)
	m.Write(msg)
	computed := hex.EncodeToString(m.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(computed), []byte(sigHex)) == 1
}

// EventInfo is a platform-neutral parsed webhook event.
type EventInfo struct {
	Type   string
	Title  string
	Body   string
	URL    string
	Labels []string
}

// ParseEvent normalizes a platform payload into an EventInfo.
func ParseEvent(platform string, payload map[string]any) EventInfo {
	m := func(v any) map[string]any {
		if mm, ok := v.(map[string]any); ok {
			return mm
		}
		return map[string]any{}
	}
	s := func(v any) string {
		if ss, ok := v.(string); ok {
			return ss
		}
		return ""
	}
	labelsOf := func(v any) []string {
		var out []string
		if arr, ok := v.([]any); ok {
			for _, l := range arr {
				out = append(out, s(m(l)["name"]))
			}
		}
		return out
	}
	switch platform {
	case "github":
		action := s(payload["action"])
		pr := m(payload["pull_request"])
		issue := m(payload["issue"])
		obj := pr
		etype := "push"
		if len(pr) > 0 {
			etype = "pull_request"
		} else if len(issue) > 0 {
			obj = issue
			etype = "issue"
		}
		t := etype
		if action != "" {
			t = etype + "." + action
		}
		return EventInfo{Type: t, Title: s(obj["title"]), Body: s(obj["body"]), URL: s(obj["html_url"]), Labels: labelsOf(obj["labels"])}
	case "linear":
		data := m(payload["data"])
		t := s(payload["type"])
		if t == "" {
			t = "issue"
		}
		a := s(payload["action"])
		if a == "" {
			a = "created"
		}
		return EventInfo{Type: t + "." + a, Title: s(data["title"]), Body: s(data["description"]), URL: s(data["url"]), Labels: labelsOf(data["labels"])}
	default:
		body := s(payload["body"])
		if body == "" {
			body = s(payload["text"])
		}
		t := s(payload["type"])
		if t == "" {
			t = "generic"
		}
		return EventInfo{Type: t, Title: s(payload["title"]), Body: body, URL: s(payload["url"])}
	}
}

// EventMatches reports whether event_type matches a rule pattern (shell-glob).
func EventMatches(pattern, eventType string) bool {
	if pattern == "" {
		return false
	}
	ok, _ := filepath.Match(pattern, eventType)
	return ok
}

// Rule is a trigger rule.
type Rule struct {
	ID           string   `json:"id"`
	Platform     string   `json:"platform"`
	Event        string   `json:"event"`
	Profile      string   `json:"profile"`
	Action       string   `json:"action"`
	FilterLabels []string `json:"filter_labels"`
}

// CreateRule persists a trigger rule.
func CreateRule(db *store.DB, platform, event, profile, action string, labels []string) (*Rule, error) {
	id := uuid.NewString()[:12]
	lj, _ := json.Marshal(labels)
	if labels == nil {
		lj = []byte("[]")
	}
	if _, err := db.Exec(`INSERT INTO trigger_rules(id,platform,event,profile,action,filter_labels,created_at,enabled) VALUES(?,?,?,?,?,?,?,1)`,
		id, platform, event, profile, action, string(lj), now()); err != nil {
		return nil, err
	}
	return &Rule{ID: id, Platform: platform, Event: event, Profile: profile, Action: action, FilterLabels: labels}, nil
}

// ListRules returns all rules (optionally platform-filtered).
func ListRules(db *store.DB, platform string) ([]Rule, error) {
	q := `SELECT id,platform,event,profile,action,filter_labels FROM trigger_rules`
	var args []any
	if platform != "" {
		q += ` WHERE platform=?`
		args = append(args, platform)
	}
	q += ` ORDER BY created_at`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var lj string
		if err := rows.Scan(&r.ID, &r.Platform, &r.Event, &r.Profile, &r.Action, &lj); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(lj), &r.FilterLabels)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MatchRules returns enabled rules for the platform whose event pattern matches
// and (if set) whose label filter intersects the event's labels.
func MatchRules(db *store.DB, platform, eventType string, info EventInfo) ([]Rule, error) {
	all, err := ListRules(db, platform)
	if err != nil {
		return nil, err
	}
	labelSet := map[string]bool{}
	for _, l := range info.Labels {
		labelSet[l] = true
	}
	var matched []Rule
	for _, r := range all {
		if !EventMatches(r.Event, eventType) {
			continue
		}
		if len(r.FilterLabels) > 0 {
			hit := false
			for _, fl := range r.FilterLabels {
				if labelSet[fl] {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		matched = append(matched, r)
	}
	return matched, nil
}

func buildTaskText(platform, eventType string, info EventInfo) string {
	parts := []string{strings.TrimSpace("Webhook " + platform + " " + eventType)}
	if info.Title != "" {
		parts = append(parts, "Title: "+info.Title)
	}
	if info.Body != "" {
		parts = append(parts, info.Body)
	}
	if info.URL != "" {
		parts = append(parts, "URL: "+info.URL)
	}
	return strings.Join(parts, "\n\n")
}

// Handler builds the webhook HTTP mux. When secret is empty, events are only
// accepted if allowUnsigned is true (an explicit operator opt-in); otherwise
// every event POST is rejected 401 so unauthenticated callers cannot inject
// queue jobs.
func Handler(db *store.DB, secret string, allowUnsigned bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, 200, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/webhooks/rules", func(w http.ResponseWriter, r *http.Request) {
		rules, _ := ListRules(db, "")
		sendJSON(w, 200, rules)
	})
	mux.HandleFunc("/webhook/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSON(w, 405, map[string]any{"error": "method not allowed"})
			return
		}
		platform := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/webhook/"))
		if platform == "" {
			sendJSON(w, 404, map[string]any{"error": "unknown path"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			sendJSON(w, 400, map[string]any{"error": "read error"})
			return
		}
		sig := firstHeader(r, "X-Hub-Signature-256", "X-Linear-Signature", "X-Slack-Signature")
		ts := r.Header.Get("X-Slack-Request-Timestamp")
		valid := VerifySignature(platform, body, sig, secret, ts)
		if secret == "" {
			// No secret configured: refuse unless the operator explicitly opted in
			// to unauthenticated events. This prevents anonymous job injection.
			if !allowUnsigned {
				sendJSON(w, 401, map[string]any{"error": "webhook receiver requires an HMAC secret; set --secret/TAG_WEBHOOK_SECRET or start with --allow-unsigned"})
				return
			}
		} else if !valid {
			sendJSON(w, 401, map[string]any{"error": "invalid signature"})
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			sendJSON(w, 400, map[string]any{"error": "invalid JSON"})
			return
		}
		info := ParseEvent(platform, payload)
		rules, _ := MatchRules(db, platform, info.Type, info)
		var ruleIDs []string
		for _, rl := range rules {
			ruleIDs = append(ruleIDs, rl.ID)
		}
		eventID := uuid.NewString()[:12]
		ridJSON, _ := json.Marshal(ruleIDs)
		if ruleIDs == nil {
			ridJSON = []byte("[]")
		}
		validInt := 0
		if valid {
			validInt = 1
		}
		// The webhook_events row is the record of dispatch; if it cannot be
		// persisted the event was not processed, so surface a 500 instead of
		// falsely reporting a successful dispatch.
		if _, err := db.Exec(`INSERT INTO webhook_events(id,platform,event_type,payload_json,received_at,signature_valid,matched_rules,status)
			VALUES(?,?,?,?,?,?,?,?)`, eventID, platform, info.Type, string(body), now(), validInt, string(ridJSON), "processed"); err != nil {
			sendJSON(w, 500, map[string]any{"error": "failed to record webhook event"})
			return
		}
		// enqueue a queue job for each matched rule (worker launch is runtime; the row is the dispatch of record)
		enqueued := 0
		for _, rl := range rules {
			jobID := uuid.NewString()[:8]
			if _, err := db.Exec(`INSERT INTO queue_jobs(id,profile,task,task_type,status,created_at) VALUES(?,?,?,?,'queued',?)`,
				jobID, rl.Profile, buildTaskText(platform, info.Type, info), strOr(rl.Action, "mixed"), now()); err != nil {
				// A matched rule failed to enqueue: the dispatch is incomplete, so
				// return 500 rather than reporting a clean success.
				sendJSON(w, 500, map[string]any{"error": "failed to enqueue job for matched rule", "event_id": eventID, "enqueued": enqueued})
				return
			}
			enqueued++
		}
		sendJSON(w, 200, map[string]any{"event_id": eventID, "rules_matched": len(rules), "signature_valid": valid})
	})
	return mux
}

func strOr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

func firstHeader(r *http.Request, keys ...string) string {
	for _, k := range keys {
		if v := r.Header.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func sendJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// Serve starts the webhook receiver (blocking).
func Serve(db *store.DB, host string, port int, secret string, allowUnsigned bool) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	fmt.Printf("TAG webhook server: http://%s  (Ctrl+C to stop)\n", addr)
	if secret == "" && allowUnsigned {
		fmt.Println("WARNING: running with --allow-unsigned and no secret — events are UNAUTHENTICATED and can enqueue jobs.")
	}
	return (&http.Server{Addr: addr, Handler: Handler(db, secret, allowUnsigned)}).ListenAndServe()
}
