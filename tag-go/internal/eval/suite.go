// Package eval implements the YAML-driven eval suite runner behind `tag eval
// run` (PRD-027, port of src/tag/eval_framework.py + cmd/marketplace.py:cmd_eval).
//
// A suite is a YAML mapping with a `cases` list; each case has an input prompt
// and deterministic expectations (expect_contains / expect_not_contains /
// expect_regex / min_length / max_length / expected_output). Cases are executed
// through the native agent loop (internal/agent), scored, and persisted to
// eval_runs / eval_cases so `tag eval list` and `tag eval show` can read them.
//
// Like internal/evaljudge and internal/benchmark this package is decoupled from
// internal/store (it takes a *sql.DB) and self-ensures its own tables, so it
// never has to edit schema.sql.
package eval

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Case is one eval case. The field set mirrors eval_framework's documented
// suite format; `prompt` is accepted as an alias for `input` because
// internal/ciauto.Case already accepted it and eval-ci suites in the wild use it.
type Case struct {
	ID               string
	Input            string
	ExpectContains   []string
	ExpectNotContain []string
	ExpectRegex      []string
	MinLength        *int
	MaxLength        *int
	// ExpectedOutput comes from `eval-dataset export` (eval_dataset_cases.
	// expected_output). Python's score_case ignores it entirely, which makes
	// every dataset-derived suite pass unconditionally; see score.go.
	ExpectedOutput *string
	// Reference is an optional rubric handed to the LLM judge (--judge).
	Reference string

	// regexes are compiled at load time so a bad pattern is a suite error, not a
	// silent per-case failure at scoring time.
	regexes []*regexp.Regexp
}

// Suite is a parsed eval suite.
type Suite struct {
	Name        string
	Description string
	Cases       []Case
}

// knownCaseKeys are the case fields the runner understands. An unrecognized key
// is rejected rather than silently ignored: a typo'd `expect_contain` would
// otherwise leave the case with zero checks and pass unconditionally.
var knownCaseKeys = map[string]bool{
	"id": true, "input": true, "prompt": true,
	"expect_contains": true, "expect_not_contains": true, "expect_regex": true,
	"min_length": true, "max_length": true,
	"expected_output": true, "reference": true, "reference_context": true,
	// tolerated, purely informational
	"description": true, "tags": true, "metadata": true,
}

// LoadSuite reads and validates a suite YAML file. Validation mirrors
// eval_framework.load_suite (mapping / non-empty `cases` / list-of-string and
// integer field types, with bool explicitly rejected for the length bounds) and
// adds two checks Python lacks: regex patterns are compiled, and unknown case
// keys are rejected.
func LoadSuite(path string) (*Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("suite not found: %s", path)
		}
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("suite is not valid YAML: %w", err)
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("suite must be a YAML mapping, got: %T", doc)
	}
	rawCases, ok := top["cases"]
	if !ok {
		return nil, fmt.Errorf("suite must have a 'cases' list")
	}
	list, ok := rawCases.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("suite must have at least one case")
	}

	s := &Suite{Name: str(top["name"]), Description: str(top["description"])}
	seen := map[string]bool{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("case %d must be a mapping, got: %T", i, item)
		}
		c, err := parseCase(i, m)
		if err != nil {
			return nil, err
		}
		if seen[c.ID] {
			// Python generates ids with cases.index(case), which returns the index
			// of the first EQUAL element, so duplicate cases collide silently and
			// `eval show` cannot tell them apart. Reject the ambiguity instead.
			return nil, fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		s.Cases = append(s.Cases, c)
	}
	return s, nil
}

func parseCase(i int, m map[string]any) (Case, error) {
	c := Case{}
	label := any(i)
	if v, ok := m["id"]; ok {
		label = v
	}
	for k := range m {
		if !knownCaseKeys[k] {
			return c, fmt.Errorf("case %v has unrecognized key %q", label, k)
		}
	}
	c.ID = str(m["id"])
	if c.ID == "" {
		c.ID = fmt.Sprintf("case_%d", i+1)
	}
	c.Input = str(m["input"])
	if c.Input == "" {
		c.Input = str(m["prompt"])
	}
	c.Reference = str(m["reference"])
	if c.Reference == "" {
		c.Reference = str(m["reference_context"])
	}
	if v, ok := m["expected_output"]; ok && v != nil {
		e := str(v)
		c.ExpectedOutput = &e
	}

	var err error
	if c.ExpectContains, err = strList(label, "expect_contains", m); err != nil {
		return c, err
	}
	if c.ExpectNotContain, err = strList(label, "expect_not_contains", m); err != nil {
		return c, err
	}
	if c.ExpectRegex, err = strList(label, "expect_regex", m); err != nil {
		return c, err
	}
	for _, p := range c.ExpectRegex {
		re, cerr := regexp.Compile(p)
		if cerr != nil {
			return c, fmt.Errorf("case %v field 'expect_regex' has an invalid pattern %q: %w", label, p, cerr)
		}
		c.regexes = append(c.regexes, re)
	}
	if c.MinLength, err = intField(label, "min_length", m); err != nil {
		return c, err
	}
	if c.MaxLength, err = intField(label, "max_length", m); err != nil {
		return c, err
	}
	return c, nil
}

func strList(label any, key string, m map[string]any) ([]string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("case %v field %q must be a list of strings", label, key)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("case %v field %q must be a list of strings", label, key)
		}
		out = append(out, s)
	}
	return out, nil
}

func intField(label any, key string, m map[string]any) (*int, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	// bool is not an int (Python rejects it explicitly because bool subclasses int).
	n, ok := v.(int)
	if !ok {
		return nil, fmt.Errorf("case %v field %q must be an integer", label, key)
	}
	return &n, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
