// Package benchmark runs a suite of prompt cases through the native agent loop
// (internal/agent + internal/llm) and scores each case pass/fail by checking for
// an expected substring in the model's final text. It defaults to the offline
// `echo` provider so it is fully exercisable without API keys. Results are
// persisted to a self-ensured `benchmark_runs` SQLite table.
//
// This package is intentionally decoupled from internal/store: it operates on a
// *sql.DB (store.DB embeds *sql.DB, so callers pass db.DB), and it never edits
// schema.sql — it ensures its own table via CREATE TABLE IF NOT EXISTS.
package benchmark

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

//go:embed suite.yaml
var defaultSuiteYAML []byte

// Case is one benchmark prompt with an optional expected substring. An empty
// Expected always passes (smoke case — just checks the loop runs).
type Case struct {
	ID       string `yaml:"id" json:"id"`
	Prompt   string `yaml:"prompt" json:"prompt"`
	Expected string `yaml:"expected" json:"expected"`
}

// Suite is a named collection of cases.
type Suite struct {
	Name  string `yaml:"-" json:"name"`
	Cases []Case `yaml:"cases" json:"cases"`
}

// LoadSuite loads a suite from a YAML file path. An empty path loads the
// embedded default suite (labelled "default").
func LoadSuite(path string) (*Suite, error) {
	var data []byte
	name := "default"
	if path == "" {
		data = defaultSuiteYAML
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read suite: %w", err)
		}
		data = b
		name = path
	}
	var s Suite
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse suite: %w", err)
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("suite %q has no cases", name)
	}
	s.Name = name
	return &s, nil
}

// CaseResult is the scored outcome of one case.
type CaseResult struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Expected string `json:"expected"`
	Output   string `json:"output"`
	Pass     bool   `json:"pass"`
}

// RunResult is the full outcome of a suite run (also the persisted record).
type RunResult struct {
	ID        string       `json:"id"`
	CreatedAt string       `json:"created_at"`
	Provider  string       `json:"provider"`
	Model     string       `json:"model"`
	Suite     string       `json:"suite"`
	Total     int          `json:"total"`
	Passed    int          `json:"passed"`
	Failed    int          `json:"failed"`
	Cases     []CaseResult `json:"cases"`
}

// Runner executes suites through the native agent loop.
type Runner struct {
	DB       *sql.DB
	Provider llm.Provider
	Model    string
	MaxSteps int
}

// EnsureSchema self-ensures the benchmark_runs table (never touches schema.sql).
func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS benchmark_runs (
		  id           TEXT PRIMARY KEY,
		  created_at   TEXT NOT NULL,
		  provider     TEXT NOT NULL,
		  model        TEXT,
		  suite        TEXT NOT NULL,
		  total        INTEGER NOT NULL,
		  passed       INTEGER NOT NULL,
		  failed       INTEGER NOT NULL,
		  results_json TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_benchmark_runs_created ON benchmark_runs(created_at);`)
	return err
}

// Run executes every case in the suite, scores it, persists the run (if DB is
// set) and returns the aggregate result.
func (r *Runner) Run(ctx context.Context, s *Suite) (*RunResult, error) {
	if r.Provider == nil {
		return nil, fmt.Errorf("benchmark: no provider set")
	}
	loop := &agent.Loop{Provider: r.Provider}
	res := &RunResult{
		ID:        uuid.NewString()[:12],
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Provider:  r.Provider.Name(),
		Model:     r.Model,
		Suite:     s.Name,
		Total:     len(s.Cases),
	}
	for _, c := range s.Cases {
		out, err := loop.Run(ctx, c.Prompt, agent.Options{Model: r.Model, MaxSteps: r.MaxSteps})
		cr := CaseResult{ID: c.ID, Prompt: c.Prompt, Expected: c.Expected}
		if err != nil {
			cr.Output = "ERROR: " + err.Error()
			cr.Pass = false
		} else {
			cr.Output = out.FinalText
			cr.Pass = c.Expected == "" || strings.Contains(out.FinalText, c.Expected)
		}
		if cr.Pass {
			res.Passed++
		} else {
			res.Failed++
		}
		res.Cases = append(res.Cases, cr)
	}
	if r.DB != nil {
		if err := r.persist(res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (r *Runner) persist(res *RunResult) error {
	if err := EnsureSchema(r.DB); err != nil {
		return err
	}
	blob, err := json.Marshal(res.Cases)
	if err != nil {
		return err
	}
	_, err = r.DB.Exec(
		`INSERT INTO benchmark_runs(id,created_at,provider,model,suite,total,passed,failed,results_json)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		res.ID, res.CreatedAt, res.Provider, res.Model, res.Suite,
		res.Total, res.Passed, res.Failed, string(blob))
	return err
}

// List returns recent runs (newest first) without their per-case detail.
func List(db *sql.DB, limit int) ([]RunResult, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(
		`SELECT id,created_at,provider,COALESCE(model,''),suite,total,passed,failed
		 FROM benchmark_runs ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunResult{}
	for rows.Next() {
		var r RunResult
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.Provider, &r.Model, &r.Suite,
			&r.Total, &r.Passed, &r.Failed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Show returns a single run (with per-case detail) resolved by an unambiguous
// id prefix. Errors if no match or multiple matches.
func Show(db *sql.DB, idPrefix string) (*RunResult, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT id,created_at,provider,COALESCE(model,''),suite,total,passed,failed,results_json
		 FROM benchmark_runs WHERE id LIKE ? || '%' ORDER BY id`, idPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []RunResult
	for rows.Next() {
		var r RunResult
		var blob string
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.Provider, &r.Model, &r.Suite,
			&r.Total, &r.Passed, &r.Failed, &blob); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(blob), &r.Cases)
		matches = append(matches, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("benchmark run not found: %q", idPrefix)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous benchmark run id %q matches %d runs", idPrefix, len(matches))
	}
}
