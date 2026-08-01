package memory

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/sqlutil"
)

// PRD-065 — post-run memory extraction.
//
// Design constraints that shaped this file:
//
//   - OFFLINE-HONEST. The extractor asks an LLM for a strict JSON array of
//     facts. Anything that is not a JSON array is treated as "no facts", never
//     as prose to be salvaged. With `--provider echo` (the offline default) the
//     model echoes the prompt back, that is not a JSON array, and the honest
//     result is "Extracted 0 memories" — exactly what the manual command
//     reported before this change. No fabrication.
//   - NO SURPRISE COST. Nothing here runs unless the caller opted in
//     (`mem2 extract`, or `tag run --auto-memorize` / memory.auto_extract).
//   - NEVER BREAK THE PARENT RUN. Callers treat every error as a warning; the
//     rate limiter and the timeout are inside the extractor so an auto-trigger
//     cannot turn into a cost or latency incident.
const (
	// ExtractPromptVersion is bumped on any prompt change so historical
	// extraction rows stay auditable (PRD-065 NFR-07).
	ExtractPromptVersion = "fact_retrieval/v1.0"

	// SourceAutoExtract tags every memory written by the extractor.
	SourceAutoExtract = "auto_extract"

	// ExtractRateLimitPerHour caps extractions per profile per hour, so a bug in
	// a post-run hook cannot loop into an LLM bill (PRD-065 §11.5).
	ExtractRateLimitPerHour = 10

	// DefaultExtractTimeout bounds the extraction LLM call.
	DefaultExtractTimeout = 30 * time.Second
)

// FactRetrievalPrompt is the versioned system prompt for phase 1.
const FactRetrievalPrompt = `[PROMPT VERSION: ` + ExtractPromptVersion + `]
You are a CLI Knowledge Organizer for the TAG software-development assistant.
Extract durable, reusable facts from the conversation below.

Focus only on: project conventions, technical decisions, gotchas and error
patterns, environment facts (names of env vars only, NEVER values), and the
file/module map.

Rules:
- Extract ONLY facts useful in FUTURE sessions. Skip ephemeral details.
- Each fact must be self-contained.
- If there are no extractable facts, return an empty array.
- NEVER extract passwords, API keys, tokens, or secret values.
- Return ONLY a JSON array. No prose, no markdown fence, no explanation.

Each element: {"text": string, "memory_type": "convention"|"decision"|"gotcha"|"fact"|"other", "confidence": number in (0,1]}`

// ExtractOptions configures one extraction.
type ExtractOptions struct {
	Profile  string
	Model    string
	DryRun   bool
	MaxTurns int
	Timeout  time.Duration
	// SkipRateLimit is set by tests and by explicitly-requested manual runs that
	// have already been counted. Auto-triggers must leave it false.
	SkipRateLimit bool
}

// Operation classifies what happened to one candidate fact.
const (
	OpAdd      = "ADD"
	OpNoop     = "NOOP"
	OpRedacted = "REDACTED"
	OpRejected = "REJECTED"
)

// ExtractedFact is one reconciled candidate.
type ExtractedFact struct {
	Text       string  `json:"text"`
	MemoryType string  `json:"memory_type"`
	Confidence float64 `json:"confidence"`
	Operation  string  `json:"operation"`
	MemoryID   string  `json:"memory_id,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// ExtractResult is the full outcome of one extraction.
type ExtractResult struct {
	ExtractionID   string          `json:"extraction_id"`
	RunID          string          `json:"run_id"`
	Profile        string          `json:"profile"`
	Model          string          `json:"model"`
	Provider       string          `json:"provider"`
	DryRun         bool            `json:"dry_run"`
	TurnsProcessed int             `json:"turns_processed"`
	Facts          []ExtractedFact `json:"facts"`
	Added          int             `json:"added"`
	Skipped        int             `json:"skipped"`
	Redacted       int             `json:"redacted"`
	DurationMS     int64           `json:"duration_ms"`
	// Note explains a degraded outcome in plain words (empty when the model
	// returned a well-formed fact array). This is what keeps an offline run
	// honest instead of silently reporting a clean zero.
	Note string `json:"note,omitempty"`
}

// EnsureExtractSchema creates the extraction audit table idempotently.
//
// It deliberately lives here rather than in the shared migrate/schema.sql so the
// memory subsystem can evolve its own tables without touching a file every other
// subsystem also writes. Safe to call on every command.
func EnsureExtractSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_extraction_runs (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			profile TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			started_at TEXT NOT NULL,
			finished_at TEXT,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			turns_processed INTEGER NOT NULL DEFAULT 0,
			facts_found INTEGER NOT NULL DEFAULT 0,
			facts_added INTEGER NOT NULL DEFAULT 0,
			facts_skipped INTEGER NOT NULL DEFAULT 0,
			redacted_count INTEGER NOT NULL DEFAULT 0,
			note TEXT NOT NULL DEFAULT '',
			error_msg TEXT,
			dry_run INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mer_profile ON memory_extraction_runs(profile, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_mer_run ON memory_extraction_runs(run_id)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// ExtractionRun is one audit row.
type ExtractionRun struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	Profile   string `json:"profile"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
	Duration  int64  `json:"duration_ms"`
	Found     int    `json:"facts_found"`
	Added     int    `json:"facts_added"`
	Skipped   int    `json:"facts_skipped"`
	Redacted  int    `json:"redacted_count"`
	Note      string `json:"note,omitempty"`
	DryRun    bool   `json:"dry_run"`
}

// ListExtractions returns recent extraction runs, newest first.
func ListExtractions(db *sql.DB, profile string, limit int) ([]ExtractionRun, error) {
	if err := EnsureExtractSchema(db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id,run_id,profile,model,provider,status,started_at,duration_ms,
			facts_found,facts_added,facts_skipped,redacted_count,note,dry_run
		FROM memory_extraction_runs`
	var args []any
	if profile != "" {
		q += ` WHERE profile=?`
		args = append(args, profile)
	}
	q += ` ORDER BY started_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so an empty history marshals to [] rather than null.
	out := []ExtractionRun{}
	for rows.Next() {
		var r ExtractionRun
		var dry int
		if err := rows.Scan(&r.ID, &r.RunID, &r.Profile, &r.Model, &r.Provider, &r.Status, &r.StartedAt,
			&r.Duration, &r.Found, &r.Added, &r.Skipped, &r.Redacted, &r.Note, &dry); err != nil {
			return nil, err
		}
		r.DryRun = dry != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadTranscript assembles the conversation text for a run, resolving RUN_ID as
// an id prefix (the same resolution `runs show` uses). Returns the resolved id,
// the transcript, and the number of turns it contains.
func LoadTranscript(db *sql.DB, runIDPrefix string, maxTurns int) (string, string, int, error) {
	var runID, prompt string
	err := db.QueryRow(`SELECT id, prompt FROM runs WHERE id LIKE ?||'%' ESCAPE '\' ORDER BY created_at DESC LIMIT 1`,
		sqlutil.EscapeLike(runIDPrefix)).Scan(&runID, &prompt)
	if err == sql.ErrNoRows {
		return "", "", 0, fmt.Errorf("Run not found: %q", runIDPrefix)
	}
	if err != nil {
		return "", "", 0, err
	}
	var turns []string
	if strings.TrimSpace(prompt) != "" {
		turns = append(turns, "user: "+prompt)
	}
	rows, err := db.Query(`SELECT output FROM steps WHERE run_id=? ORDER BY id`, runID)
	if err != nil {
		return runID, "", 0, err
	}
	for rows.Next() {
		var o sql.NullString
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return runID, "", 0, err
		}
		if o.Valid && strings.TrimSpace(o.String) != "" {
			turns = append(turns, "assistant: "+o.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return runID, "", 0, err
	}
	if maxTurns > 0 && len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}
	return runID, strings.Join(turns, "\n"), len(turns), nil
}

// secretPatterns is the extractor's inline pre-write redaction suite. It mirrors
// the high-signal patterns in internal/security (which only scans FILES, not
// strings) so a credential that appeared in a transcript can never be persisted
// into the memory store. PRD-065 FR-07 / AC-05.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9\-_]{20,}`),
	regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`gh[opusr]_[0-9A-Za-z]{36}`),
	regexp.MustCompile(`github_pat_[0-9A-Za-z_]{59,}`),
	regexp.MustCompile(`xox[baprs]-[0-9A-Za-z\-]{10,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY`),
	regexp.MustCompile(`(?i)(secret|token|password|api[_-]?key)\s*[=:]\s*["']?[A-Za-z0-9/+_\-]{12,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`),
}

// ContainsSecret reports whether text matches any credential pattern.
func ContainsSecret(text string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// injectionPatterns catch a transcript trying to steer the extractor into
// writing attacker-chosen memories (PRD-065 §11.2).
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\badd (this )?to memory\b`),
	regexp.MustCompile(`(?i)\bremember that\b`),
	regexp.MustCompile(`(?i)\bstore the following\b`),
	regexp.MustCompile(`(?i)\bignore (all )?previous instructions\b`),
}

func looksLikeInjection(text string) bool {
	for _, re := range injectionPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

func contentMD5(s string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:])
}

// ExtractRateLimitError is returned when the per-profile hourly cap is hit.
type ExtractRateLimitError struct {
	Profile string
	Limit   int
}

func (e ExtractRateLimitError) Error() string {
	return fmt.Sprintf("extraction rate limit reached (%d/hour for profile %q)", e.Limit, e.Profile)
}

// Extract runs the post-run extraction pipeline for one run.
//
// Errors are returned, never printed: the caller decides whether an extraction
// failure is fatal (it never is for an auto-trigger).
func Extract(ctx context.Context, db *sql.DB, prov llm.Provider, runIDPrefix string, opts ExtractOptions) (*ExtractResult, error) {
	if prov == nil {
		return nil, fmt.Errorf("no LLM provider available for extraction")
	}
	if err := EnsureExtractSchema(db); err != nil {
		return nil, err
	}
	profile := opts.Profile
	if profile == "" {
		profile = "default"
	}
	if !opts.DryRun && !opts.SkipRateLimit {
		since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM memory_extraction_runs WHERE profile=? AND dry_run=0 AND started_at >= ?`,
			profile, since).Scan(&n); err != nil {
			return nil, err
		}
		if n >= ExtractRateLimitPerHour {
			return nil, ExtractRateLimitError{Profile: profile, Limit: ExtractRateLimitPerHour}
		}
	}

	runID, transcript, turns, err := LoadTranscript(db, runIDPrefix, opts.MaxTurns)
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultExtractTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := &ExtractResult{
		ExtractionID:   "extr_" + uuid.NewString()[:12],
		RunID:          runID,
		Profile:        profile,
		Model:          opts.Model,
		Provider:       prov.Name(),
		DryRun:         opts.DryRun,
		TurnsProcessed: turns,
		Facts:          []ExtractedFact{},
	}
	started := time.Now()
	startedAt := time.Now().UTC().Format(time.RFC3339)

	raw, err := completeText(callCtx, prov, llm.Request{
		Model:     opts.Model,
		MaxTokens: 2048,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: FactRetrievalPrompt},
			{Role: llm.RoleUser, Content: "CONVERSATION:\n" + transcript},
		},
	})
	if err != nil {
		res.DurationMS = time.Since(started).Milliseconds()
		res.Note = fmt.Sprintf("extractor model call failed: %v — no memories were written", err)
		_ = recordExtraction(db, res, startedAt, "failed", err.Error())
		return res, err
	}

	candidates, parseNote := parseCandidates(raw)
	res.Note = parseNote

	existing, err := loadExistingContentHashes(db, profile)
	if err != nil {
		return nil, err
	}

	for _, c := range candidates {
		f := ExtractedFact{Text: c.Text, MemoryType: c.MemoryType, Confidence: c.Confidence}
		switch {
		case ContainsSecret(c.Text):
			f.Operation = OpRedacted
			f.Reason = "matched a credential pattern; dropped before write"
			res.Redacted++
		case looksLikeInjection(c.Text):
			f.Operation = OpRejected
			f.Reason = "matched a prompt-injection pattern; dropped before write"
			res.Skipped++
		case existing[contentMD5(c.Text)]:
			f.Operation = OpNoop
			f.Reason = "already stored (exact duplicate)"
			res.Skipped++
		default:
			f.Operation = OpAdd
			if !opts.DryRun {
				id, aerr := AddWithSource(db, profile, c.Text, c.MemoryType, c.Confidence, SourceAutoExtract)
				if aerr != nil {
					f.Operation = OpRejected
					f.Reason = aerr.Error()
					res.Skipped++
					break
				}
				f.MemoryID = id
			}
			existing[contentMD5(c.Text)] = true
			res.Added++
		}
		res.Facts = append(res.Facts, f)
	}
	res.DurationMS = time.Since(started).Milliseconds()
	status := "done"
	if opts.DryRun {
		status = "dry_run"
	}
	if err := recordExtraction(db, res, startedAt, status, ""); err != nil {
		return res, err
	}
	return res, nil
}

func recordExtraction(db *sql.DB, r *ExtractResult, startedAt, status, errMsg string) error {
	dry := 0
	if r.DryRun {
		dry = 1
	}
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, err := db.Exec(`INSERT INTO memory_extraction_runs
		(id,run_id,profile,model,provider,status,started_at,finished_at,duration_ms,turns_processed,
		 facts_found,facts_added,facts_skipped,redacted_count,note,error_msg,dry_run)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ExtractionID, r.RunID, r.Profile, r.Model, r.Provider, status, startedAt,
		time.Now().UTC().Format(time.RFC3339), r.DurationMS, r.TurnsProcessed,
		len(r.Facts), r.Added, r.Skipped, r.Redacted, r.Note, errVal, dry)
	return err
}

func loadExistingContentHashes(db *sql.DB, profile string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT content FROM semantic_memories WHERE profile=?`, profile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[contentMD5(c)] = true
	}
	return out, rows.Err()
}

type candidateFact struct {
	Text       string  `json:"text"`
	MemoryType string  `json:"memory_type"`
	Confidence float64 `json:"confidence"`
}

// parseCandidates decodes a STRICT JSON array of facts. Anything else yields no
// candidates plus a note saying so — the extractor never invents memories from
// prose, which is what keeps `--provider echo` honest.
func parseCandidates(raw string) ([]candidateFact, string) {
	s := strings.TrimSpace(raw)
	// Tolerate a ```json fence, which well-behaved models still sometimes emit.
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if s == "" {
		return nil, "extractor model returned an empty response — no memories were extracted"
	}
	if !strings.HasPrefix(s, "[") {
		return nil, "extractor model did not return a JSON fact array (offline/echo providers cannot extract) — no memories were extracted"
	}
	var arr []candidateFact
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil, fmt.Sprintf("extractor model returned malformed JSON (%v) — no memories were extracted", err)
	}
	out := make([]candidateFact, 0, len(arr))
	for _, c := range arr {
		c.Text = strings.TrimSpace(c.Text)
		if c.Text == "" {
			continue
		}
		if _, ok := halfLives[c.MemoryType]; !ok {
			c.MemoryType = "fact"
		}
		if c.Confidence <= 0 || c.Confidence > 1 {
			c.Confidence = 0.8
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, "extractor model returned no facts"
	}
	return out, ""
}

// completeText drains a provider stream into its text, surfacing a stream error
// rather than silently returning a truncated string.
func completeText(ctx context.Context, prov llm.Provider, req llm.Request) (string, error) {
	ch, err := prov.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var streamErr error
	for ev := range ch {
		switch ev.Type {
		case llm.EventTextDelta:
			b.WriteString(ev.Text)
		case llm.EventError:
			if ev.Err != nil {
				streamErr = ev.Err
			}
		}
	}
	if streamErr != nil {
		return "", streamErr
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return b.String(), nil
}
