// Package evaljudge implements LLM-as-judge scoring (parity roadmap #527 bucket
// B, port of src/tag/eval_judge.py). A judge model is asked to score a candidate
// answer against a question and an optional reference/rubric, and returns a
// JSON verdict {score, passed, reasoning}. It drives the model through the native
// agent loop (internal/agent + internal/llm) and defaults to the offline `echo`
// provider so it is fully exercisable without API keys — with echo the judgment
// is deterministic (parsed from the echoed prompt, falling back to a neutral
// score). Real judging happens with --provider openai|anthropic.
//
// Like internal/benchmark, this package is decoupled from internal/store: it
// operates on a *sql.DB (store.DB embeds *sql.DB, so callers pass db.DB) and
// never edits schema.sql — it ensures its own `eval_judgments` table via
// CREATE TABLE IF NOT EXISTS.
package evaljudge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

// DefaultThreshold is the minimum score (0.0–1.0) counted as a pass, mirroring
// the Python judge's default pass threshold.
const DefaultThreshold = 0.7

// Judgment is the scored verdict for one candidate answer (also the persisted
// record). The scoring shape mirrors Python's {score, passed, reasoning}, with
// score clamped to [0.0, 1.0].
type Judgment struct {
	ID        string  `json:"id"`
	CreatedAt string  `json:"created_at"`
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	Question  string  `json:"question"`
	Answer    string  `json:"answer"`
	Reference string  `json:"reference"`
	Score     float64 `json:"score"`
	Passed    bool    `json:"passed"`
	Reasoning string  `json:"reasoning"`
	Threshold float64 `json:"threshold"`
}

// Judge scores a candidate answer against a question (and optional reference /
// rubric) using the given provider, persists the result (if db is non-nil), and
// returns the Judgment. Model may be empty (provider default). threshold <= 0
// falls back to DefaultThreshold.
func Judge(ctx context.Context, db *sql.DB, prov llm.Provider, model, question, answer, reference string, threshold float64) (*Judgment, error) {
	if prov == nil {
		return nil, fmt.Errorf("evaljudge: no provider set")
	}
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	prompt := buildJudgePrompt(question, answer, reference)
	loop := &agent.Loop{Provider: prov}
	res, err := loop.Run(ctx, prompt, agent.Options{Model: model, System: judgeSystem})
	if err != nil {
		return nil, err
	}
	score, reasoning := parseJudgeResponse(res.FinalText)
	j := &Judgment{
		ID:        uuid.NewString()[:12],
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Provider:  prov.Name(),
		Model:     model,
		Question:  question,
		Answer:    answer,
		Reference: reference,
		Score:     score,
		Passed:    score >= threshold,
		Reasoning: reasoning,
		Threshold: threshold,
	}
	if db != nil {
		if err := persist(db, j); err != nil {
			return nil, err
		}
	}
	return j, nil
}

const judgeSystem = "You are an impartial evaluator. Respond ONLY with a valid JSON object " +
	`matching {"score": float, "passed": bool, "reasoning": str}.`

// buildJudgePrompt combines the question, candidate answer, and optional
// reference/rubric into a scoring prompt (mirrors _build_judge_prompt).
func buildJudgePrompt(question, answer, reference string) string {
	var b strings.Builder
	b.WriteString("Score the candidate answer from 0.0 to 1.0 for correctness and relevance.\n")
	b.WriteString(`Return JSON: {"score": float, "passed": bool, "reasoning": str}` + "\n\n")
	b.WriteString("## Question / Task\n")
	b.WriteString(question)
	b.WriteString("\n\n## Candidate Answer\n")
	b.WriteString(answer)
	if strings.TrimSpace(reference) != "" {
		b.WriteString("\n\n## Reference / Rubric\n")
		b.WriteString(reference)
	}
	b.WriteString("\n\nRespond ONLY with a valid JSON object matching the schema above.")
	return b.String()
}

// parseJudgeResponse extracts the first JSON object from judge output and
// returns a clamped score and reasoning. Mirrors _parse_judge_response: direct
// parse, then brace-scan, then a neutral {0.5, "parse error"} fallback (which is
// the deterministic offline outcome with the echo provider, since the echoed
// prompt contains no valid verdict JSON of its own).
func parseJudgeResponse(text string) (float64, string) {
	text = strings.TrimSpace(text)
	if score, reasoning, ok := decodeVerdict(text); ok {
		return score, reasoning
	}
	if start := strings.Index(text, "{"); start != -1 {
		if end := strings.LastIndex(text, "}"); end > start {
			if score, reasoning, ok := decodeVerdict(text[start : end+1]); ok {
				return score, reasoning
			}
		}
	}
	return 0.5, "parse error"
}

func decodeVerdict(s string) (float64, string, bool) {
	var v struct {
		Score     *float64 `json:"score"`
		Reasoning string   `json:"reasoning"`
		Rationale string   `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return 0, "", false
	}
	if v.Score == nil {
		return 0, "", false
	}
	score := *v.Score
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	reasoning := v.Reasoning
	if reasoning == "" {
		reasoning = v.Rationale
	}
	return score, reasoning, true
}

// EnsureSchema self-ensures the eval_judgments table (never touches schema.sql).
func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS eval_judgments (
		  id         TEXT PRIMARY KEY,
		  created_at TEXT NOT NULL,
		  provider   TEXT NOT NULL,
		  model      TEXT,
		  question   TEXT NOT NULL,
		  answer     TEXT NOT NULL,
		  reference  TEXT,
		  score      REAL NOT NULL,
		  passed     INTEGER NOT NULL,
		  reasoning  TEXT,
		  threshold  REAL NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_eval_judgments_created ON eval_judgments(created_at);`)
	return err
}

func persist(db *sql.DB, j *Judgment) error {
	if err := EnsureSchema(db); err != nil {
		return err
	}
	passed := 0
	if j.Passed {
		passed = 1
	}
	_, err := db.Exec(
		`INSERT INTO eval_judgments(id,created_at,provider,model,question,answer,reference,score,passed,reasoning,threshold)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.CreatedAt, j.Provider, j.Model, j.Question, j.Answer, j.Reference,
		j.Score, passed, j.Reasoning, j.Threshold)
	return err
}

// List returns recent judgments (newest first).
func List(db *sql.DB, limit int) ([]Judgment, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(
		`SELECT id,created_at,provider,COALESCE(model,''),question,answer,COALESCE(reference,''),score,passed,COALESCE(reasoning,''),threshold
		 FROM eval_judgments ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Judgment{}
	for rows.Next() {
		j, err := scanJudgment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Show returns a single judgment resolved by an unambiguous id prefix. Errors if
// there is no match or multiple matches.
func Show(db *sql.DB, idPrefix string) (*Judgment, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT id,created_at,provider,COALESCE(model,''),question,answer,COALESCE(reference,''),score,passed,COALESCE(reasoning,''),threshold
		 FROM eval_judgments WHERE id LIKE ? || '%' ORDER BY id`, idPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []Judgment
	for rows.Next() {
		j, err := scanJudgment(rows)
		if err != nil {
			return nil, err
		}
		matches = append(matches, *j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("judgment not found: %q", idPrefix)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous judgment id %q matches %d judgments", idPrefix, len(matches))
	}
}

func scanJudgment(rows *sql.Rows) (*Judgment, error) {
	var j Judgment
	var passed int
	if err := rows.Scan(&j.ID, &j.CreatedAt, &j.Provider, &j.Model, &j.Question,
		&j.Answer, &j.Reference, &j.Score, &passed, &j.Reasoning, &j.Threshold); err != nil {
		return nil, err
	}
	j.Passed = passed != 0
	return &j, nil
}
