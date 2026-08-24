package guardrail

// Content guardrails: input (PRD-122) and output (PRD-121) validation layered on
// the shared GuardrailResult (PRD-124). Distinct from the tripwire Processor
// (PRD-123, which screens tool I/O by stage) — these screen the model's INPUT
// and OUTPUT content with typed detectors and can block/warn/sanitize/rewrite.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

func queueHexID2(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000")))[:n]
	}
	return hex.EncodeToString(b)[:n]
}

// ---- detectors -------------------------------------------------------------

var (
	reEmailC = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	reSSNC   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	rePhoneC = regexp.MustCompile(`\b(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]\d{3}[-.\s]\d{4}\b`)
	reCCC    = regexp.MustCompile(`\b(?:\d[ -]?){13,16}\b`)
)

// DefaultInjectionPatterns are RE2-safe (no backrefs/lookaround); matched
// case-insensitively. Operators override via a rule's config "patterns".
var DefaultInjectionPatterns = []string{
	`ignore (all )?(previous|prior|above) (instructions|prompts|context)`,
	`disregard (all )?(previous|prior) (instructions|prompts)`,
	`you are now (a|an) (dan|jailbreak|unrestricted)`,
	`output your (system prompt|instructions|context)`,
	`reveal your (system prompt|instructions)`,
	`act as if you have no (restrictions|guidelines|safety)`,
	`pretend (you are|to be) (an ai without|a model without) (restrictions|safety)`,
	`\bjailbreak\b`,
	`\[system\]`,
}

// DefaultProfanity is a small, conservative seed list (word-boundary matched).
var DefaultProfanity = []string{"damn", "shit", "fuck", "bitch", "asshole", "bastard"}

func luhnOK(s string) bool {
	var nums []int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			nums = append(nums, int(c-'0'))
		}
	}
	if len(nums) < 13 {
		return false
	}
	sum := 0
	for i := 0; i < len(nums); i++ {
		n := nums[len(nums)-1-i]
		if i%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// DetectPII returns (kind,match) hits.
func DetectPII(text string) [][2]string {
	var hits [][2]string
	for _, m := range reEmailC.FindAllString(text, -1) {
		hits = append(hits, [2]string{"email", m})
	}
	for _, m := range reSSNC.FindAllString(text, -1) {
		hits = append(hits, [2]string{"SSN", m})
	}
	for _, m := range rePhoneC.FindAllString(text, -1) {
		hits = append(hits, [2]string{"phone", m})
	}
	for _, m := range reCCC.FindAllString(text, -1) {
		if luhnOK(m) {
			hits = append(hits, [2]string{"credit-card", m})
		}
	}
	return hits
}

// SanitizePII replaces PII with deterministic placeholders.
func SanitizePII(text string) string {
	text = reEmailC.ReplaceAllString(text, "[REDACTED_EMAIL]")
	text = reSSNC.ReplaceAllString(text, "[REDACTED_SSN]")
	text = rePhoneC.ReplaceAllString(text, "[REDACTED_PHONE]")
	text = reCCC.ReplaceAllStringFunc(text, func(m string) string {
		if luhnOK(m) {
			return "[REDACTED_CC]"
		}
		return m
	})
	return text
}

// SecretHits reuses the PRD-123 secret scanner so input/output share one scanner.
func SecretHits(text string) []string {
	var out []string
	for _, h := range builtinScan("secrets", text) {
		parts := strings.SplitN(h.detector, ":", 2)
		out = append(out, parts[len(parts)-1])
	}
	return out
}

func compileInjection(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		expr := p
		if !strings.HasPrefix(expr, "(?") {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("injection pattern %q is invalid: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// EvalGuardrail evaluates one content guardrail against text.
func EvalGuardrail(gtype string, action GuardrailAction, text string, config map[string]any) GuardrailResult {
	name := gtype
	switch gtype {
	case "pii":
		hits := DetectPII(text)
		if len(hits) == 0 {
			return Pass(name)
		}
		if action == GActionSanitize {
			return Sanitize(SanitizePII(text), "PII_SANITIZED", name)
		}
		kinds := map[string]bool{}
		for _, h := range hits {
			kinds[h[0]] = true
		}
		ks := make([]string, 0, len(kinds))
		for k := range kinds {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return GuardrailResult{Action: action, Guardrail: name, Reason: "PII_DETECTED:" + strings.Join(ks, ",")}
	case "secret":
		hits := SecretHits(text)
		if len(hits) == 0 {
			return Pass(name)
		}
		return GuardrailResult{Action: action, Guardrail: name, Reason: "SECRET_DETECTED:" + hits[0]}
	case "prompt-injection":
		pats := DefaultInjectionPatterns
		if raw, ok := config["patterns"].([]any); ok && len(raw) > 0 {
			pats = pats[:0]
			for _, r := range raw {
				if s, ok := r.(string); ok {
					pats = append(pats, s)
				}
			}
		}
		res, err := compileInjection(pats)
		if err != nil {
			return GuardrailResult{Action: GActionBlock, Guardrail: name, Reason: "PATTERN_ERROR:" + err.Error()}
		}
		for i, re := range res {
			if re.MatchString(text) {
				return GuardrailResult{Action: action, Guardrail: name, Reason: fmt.Sprintf("PROMPT_INJECTION:pattern_%d", i)}
			}
		}
		return Pass(name)
	case "length-limit":
		maxLen := 4096
		if v, ok := asInt2(config["max_length"]); ok {
			maxLen = v
		}
		if len([]rune(text)) > maxLen {
			return GuardrailResult{Action: action, Guardrail: name, Reason: fmt.Sprintf("INPUT_TOO_LONG:%d>%d", len([]rune(text)), maxLen)}
		}
		return Pass(name)
	case "json-schema":
		var obj any
		if err := json.Unmarshal([]byte(text), &obj); err != nil {
			return GuardrailResult{Action: action, Guardrail: name, Reason: "SCHEMA_INVALID:not JSON: " + truncErr(err.Error())}
		}
		if sc, ok := config["schema"].(map[string]any); ok {
			if e := validateJSONSchema(obj, sc); e != "" {
				return GuardrailResult{Action: action, Guardrail: name, Reason: "SCHEMA_INVALID:" + truncErr(e)}
			}
		}
		return Pass(name)
	case "profanity":
		words := DefaultProfanity
		if raw, ok := config["words"].([]any); ok && len(raw) > 0 {
			words = words[:0]
			for _, r := range raw {
				if s, ok := r.(string); ok {
					words = append(words, s)
				}
			}
		}
		low := strings.ToLower(text)
		for _, w := range words {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(strings.ToLower(w)) + `\b`).MatchString(low) {
				return GuardrailResult{Action: action, Guardrail: name, Reason: "PROFANITY_DETECTED"}
			}
		}
		return Pass(name)
	case "topic-filter", "toxicity":
		msg := "requires a classifier model (not run offline)"
		return GuardrailResult{Action: GActionPass, Guardrail: name, Message: &msg, Metadata: map[string]any{"llm_required": true}}
	default:
		msg := "unknown guardrail type " + gtype
		return GuardrailResult{Action: GActionPass, Guardrail: name, Message: &msg}
	}
}

func asInt2(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func truncErr(s string) string {
	if len(s) > 100 {
		return s[:100]
	}
	return s
}

// ---- minimal JSON-schema validation (type/required/properties/items) -------

var jsonTypeOK = map[string]func(any) bool{
	"object":  func(v any) bool { _, ok := v.(map[string]any); return ok },
	"array":   func(v any) bool { _, ok := v.([]any); return ok },
	"string":  func(v any) bool { _, ok := v.(string); return ok },
	"boolean": func(v any) bool { _, ok := v.(bool); return ok },
	"null":    func(v any) bool { return v == nil },
	"number":  func(v any) bool { _, ok := v.(float64); return ok },
	"integer": func(v any) bool { f, ok := v.(float64); return ok && f == float64(int64(f)) },
}

func validateJSONSchema(obj any, schema map[string]any) string {
	if t, ok := schema["type"].(string); ok {
		if check, known := jsonTypeOK[t]; known && !check(obj) {
			return fmt.Sprintf("expected %s", t)
		}
	}
	if m, ok := obj.(map[string]any); ok {
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if key, ok := r.(string); ok {
					if _, present := m[key]; !present {
						return fmt.Sprintf("missing required property %q", key)
					}
				}
			}
		}
		if props, ok := schema["properties"].(map[string]any); ok {
			for key, sub := range props {
				if sm, ok := sub.(map[string]any); ok {
					if val, present := m[key]; present {
						if e := validateJSONSchema(val, sm); e != "" {
							return key + ": " + e
						}
					}
				}
			}
		}
	}
	if arr, ok := obj.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for i, it := range arr {
				if e := validateJSONSchema(it, items); e != "" {
					return fmt.Sprintf("[%d]: %s", i, e)
				}
			}
		}
	}
	return ""
}

// ---- config persistence + audit -------------------------------------------

// ContentConfig is one configured content guardrail.
type ContentConfig struct {
	ID       string         `json:"id"`
	Profile  string         `json:"profile"`
	Type     string         `json:"guardrail_type"`
	Action   string         `json:"action"`
	Config   map[string]any `json:"config"`
	Severity string         `json:"severity"`
	Enabled  bool           `json:"enabled"`
}

func contentTable(direction string) string {
	if direction == "input" {
		return "input_guardrail_configs"
	}
	return "output_guardrail_configs"
}

func contentModelCol(direction string) string {
	if direction == "input" {
		return "classifier_model"
	}
	return "remediation_model"
}

// EnsureContentSchema creates the config table for a direction plus the shared
// audit log.
func EnsureContentSchema(db *sql.DB, direction string) error {
	table := contentTable(direction)
	modelCol := contentModelCol(direction)
	ddl := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  id              TEXT PRIMARY KEY,
  profile         TEXT NOT NULL,
  guardrail_type  TEXT NOT NULL,
  action          TEXT NOT NULL DEFAULT 'block',
  config_json     TEXT,
  severity        TEXT NOT NULL DEFAULT 'high',
  enabled         INTEGER NOT NULL DEFAULT 1,
  %s        TEXT,
  created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_%s_profile ON %s(profile);`, table, modelCol, table, table)
	if _, err := db.Exec(ddl); err != nil {
		return err
	}
	return EnsureSchema(db) // shared guardrail_events
}

func nowTSContent() string { return time.Now().UTC().Format(time.RFC3339) }

// AddContentConfig persists a new content guardrail; returns its id.
func AddContentConfig(db *sql.DB, direction, profile, gtype, action string, config map[string]any, severity, model string) (string, error) {
	if err := EnsureContentSchema(db, direction); err != nil {
		return "", err
	}
	id := queueHexID2(12)
	cj, _ := json.Marshal(config)
	_, err := db.Exec(fmt.Sprintf(
		"INSERT INTO %s (id,profile,guardrail_type,action,config_json,severity,enabled,%s,created_at) VALUES (?,?,?,?,?,?,1,?,?)",
		contentTable(direction), contentModelCol(direction)),
		id, profile, gtype, action, string(cj), severity, nullIfEmpty(model), nowTSContent())
	return id, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ListContentConfigs returns configs for a direction/profile in severity order.
func ListContentConfigs(db *sql.DB, direction, profile string) ([]ContentConfig, error) {
	if err := EnsureContentSchema(db, direction); err != nil {
		return nil, err
	}
	q := fmt.Sprintf("SELECT id,profile,guardrail_type,action,COALESCE(config_json,'{}'),severity,enabled FROM %s", contentTable(direction))
	var args []any
	if profile != "" {
		q += " WHERE profile=?"
		args = append(args, profile)
	}
	q += " ORDER BY CASE severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END, created_at"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContentConfig{}
	for rows.Next() {
		var c ContentConfig
		var cj string
		var enabled int
		if err := rows.Scan(&c.ID, &c.Profile, &c.Type, &c.Action, &cj, &c.Severity, &enabled); err != nil {
			return nil, err
		}
		c.Enabled = enabled == 1
		_ = json.Unmarshal([]byte(cj), &c.Config)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RemoveContentConfig deletes one config by id; reports whether a row was removed.
func RemoveContentConfig(db *sql.DB, direction, id string) (bool, error) {
	if err := EnsureContentSchema(db, direction); err != nil {
		return false, err
	}
	res, err := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE id=?", contentTable(direction)), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ContentVerdict is the collapsed result of running a content chain.
type ContentVerdict struct {
	Direction   string            `json:"direction"`
	FinalAction GuardrailAction   `json:"final_action"`
	Text        string            `json:"text"`
	Results     []GuardrailResult `json:"results"`
}

func appendContentEvent(db *sql.DB, direction, profile, runID string, r GuardrailResult) {
	if err := EnsureSchema(db); err != nil {
		return
	}
	blocked := 0
	if r.IsBlocking() {
		blocked = 1
	}
	detail := r.Reason
	if r.SanitizedText != nil {
		detail = strings.TrimSpace(detail + " " + Redact(*r.SanitizedText))
	}
	_, _ = db.Exec(`INSERT INTO guardrail_events
		(created_at, session_id, direction, stage, tool, rule, action, blocked, undecidable, detail)
		VALUES(?,?,?,?,?,?,?,?,0,?)`,
		nowTSContent(), runID, direction, r.Guardrail, profile, r.Guardrail, string(r.Action), blocked, detail)
}

// RunContentChain runs every enabled guardrail for a profile in severity order:
// block short-circuits; sanitize threads the rewritten text forward; warn/rewrite
// are recorded. Each decision is appended to guardrail_events when persist is set.
func RunContentChain(db *sql.DB, direction, profile, text, runID string, persist bool) (ContentVerdict, error) {
	cfgs, err := ListContentConfigs(db, direction, profile)
	if err != nil {
		return ContentVerdict{}, err
	}
	v := ContentVerdict{Direction: direction, FinalAction: GActionPass, Text: text, Results: []GuardrailResult{}}
	current := text
	for _, c := range cfgs {
		if !c.Enabled {
			continue
		}
		r := EvalGuardrail(c.Type, GuardrailAction(c.Action), current, c.Config)
		v.Results = append(v.Results, r)
		if persist {
			appendContentEvent(db, direction, profile, runID, r)
		}
		if r.IsBlocking() {
			v.FinalAction = r.Action
			v.Text = current
			return v, nil
		}
		if r.ShouldSanitize() {
			current = *r.SanitizedText
			if v.FinalAction == GActionPass {
				v.FinalAction = GActionSanitize
			}
		} else if (r.Action == GActionWarn || r.Action == GActionRewrite) && v.FinalAction == GActionPass {
			v.FinalAction = r.Action
		}
	}
	v.Text = current
	return v, nil
}
