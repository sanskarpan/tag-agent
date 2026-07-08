package evaljudge

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/tag-agent/tag/internal/llm"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/judge.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// verdictProvider is an offline provider that emits a fixed judge verdict JSON,
// standing in for a real judge model so the parse/scoring path is exercised
// without any API calls.
type verdictProvider struct{ json string }

func (verdictProvider) Name() string { return "verdict" }

func (p verdictProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 2)
	go func() {
		defer close(ch)
		ch <- llm.Event{Type: llm.EventTextDelta, Text: p.json}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

// TestJudgeEchoDeterministic verifies the offline echo provider yields the
// deterministic neutral fallback (score 0.5, "parse error", not passed): the
// echoed prompt contains no valid verdict JSON of its own.
func TestJudgeEchoDeterministic(t *testing.T) {
	db := openTestDB(t)
	j, err := Judge(context.Background(), db, llm.EchoProvider{}, "", "What is 2+2?", "4", "", 0)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if j.Score != 0.5 || j.Reasoning != "parse error" || j.Passed {
		t.Fatalf("echo judgment not deterministic: %+v", j)
	}
	if j.Threshold != DefaultThreshold || j.Provider != "echo" {
		t.Fatalf("unexpected metadata: %+v", j)
	}
}

// TestJudgeParsesVerdict verifies a judge model's JSON verdict is parsed into
// the {score, passed, reasoning} shape, with passed derived from the threshold.
func TestJudgeParsesVerdict(t *testing.T) {
	db := openTestDB(t)
	prov := verdictProvider{json: `{"score": 0.9, "passed": true, "reasoning": "correct and complete"}`}
	j, err := Judge(context.Background(), db, prov, "gpt-x", "Q", "A", "ref", 0.7)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if j.Score != 0.9 || !j.Passed || j.Reasoning != "correct and complete" {
		t.Fatalf("verdict not parsed: %+v", j)
	}

	// Below threshold -> not passed. Also exercises brace-scan (leading noise)
	// and the `rationale` alias fallback for reasoning.
	prov2 := verdictProvider{json: "here you go: {\"score\": 0.4, \"rationale\": \"partially wrong\"}"}
	j2, err := Judge(context.Background(), db, prov2, "", "Q", "A", "", 0.7)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if j2.Score != 0.4 || j2.Passed || j2.Reasoning != "partially wrong" {
		t.Fatalf("threshold/brace-scan wrong: %+v", j2)
	}
}

// TestScoreClamp checks scores are clamped into [0,1].
func TestScoreClamp(t *testing.T) {
	db := openTestDB(t)
	hi, err := Judge(context.Background(), db, verdictProvider{json: `{"score": 5, "reasoning": "x"}`}, "", "Q", "A", "", 0)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if hi.Score != 1.0 {
		t.Fatalf("expected clamp to 1.0, got %v", hi.Score)
	}
	lo, err := Judge(context.Background(), db, verdictProvider{json: `{"score": -2, "reasoning": "x"}`}, "", "Q", "A", "", 0)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if lo.Score != 0.0 {
		t.Fatalf("expected clamp to 0.0, got %v", lo.Score)
	}
}

// TestListShow checks persistence, List ordering, and id-prefix Show resolution.
func TestListShow(t *testing.T) {
	db := openTestDB(t)
	j, err := Judge(context.Background(), db, llm.EchoProvider{}, "", "Q1", "A1", "R1", 0)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if _, err := Judge(context.Background(), db, llm.EchoProvider{}, "", "Q2", "A2", "", 0); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	list, err := List(db, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 judgments, got %d", len(list))
	}
	got, err := Show(db, j.ID[:4])
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got.ID != j.ID || got.Question != "Q1" || got.Reference != "R1" {
		t.Fatalf("Show returned wrong record: %+v", got)
	}
}

// TestListEmpty and TestShowNotFound cover empty/absent cases.
func TestListEmpty(t *testing.T) {
	db := openTestDB(t)
	list, err := List(db, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %+v", list)
	}
}

func TestShowNotFound(t *testing.T) {
	db := openTestDB(t)
	if _, err := Show(db, "nope"); err == nil {
		t.Fatal("expected not-found error")
	}
}
