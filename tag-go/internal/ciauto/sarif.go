package ciauto

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ErrMalformedSARIF marks input that is not usable SARIF. Callers map it to a
// USAGE error (exit 2): a broken input file is the operator's mistake, not a
// crash of the tool. Port note: the Python parse_sarif raises ValueError for bad
// JSON but happily returns an empty list for JSON that is valid but is not SARIF
// at all (e.g. `[]` or `{"hello": 1}`), which reads as "scan is clean". That is
// a silent false-negative on a security gate, so this port rejects it instead.
var ErrMalformedSARIF = errors.New("malformed SARIF")

// Finding is one SARIF result location, flattened.
//
// Field names mirror src/tag/ci.py:parse_sarif (rule_id/message/path/start_line/
// severity) so JSON consumers written against the Python implementation keep
// working.
type Finding struct {
	RuleID    string `json:"rule_id"`
	Message   string `json:"message"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	Severity  string `json:"severity"`
}

// sarifSeverityRank orders SARIF levels from most to least serious. SARIF 2.1.0
// defines exactly these four; anything else sorts last.
var sarifSeverityRank = map[string]int{"error": 0, "warning": 1, "note": 2, "none": 3}

// KnownSeverities lists the SARIF 2.1.0 result levels, most serious first.
func KnownSeverities() []string { return []string{"error", "warning", "note", "none"} }

// normalizeSARIFPath turns a SARIF artifactLocation URI into a safe
// repo-relative path. `file:` URIs are percent-decoded, then every "", "." and
// ".." segment is dropped so the result can never escape the repo root once
// joined against it. Mirrors _normalize_sarif_path.
func normalizeSARIFPath(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "file:") {
		if u, err := url.Parse(raw); err == nil {
			if p, err := url.PathUnescape(u.Path); err == nil {
				raw = p
			} else {
				raw = u.Path
			}
		}
	}
	norm := path.Clean(strings.ReplaceAll(raw, `\`, "/"))
	parts := make([]string, 0, 4)
	for _, p := range strings.Split(norm, "/") {
		switch p {
		case "", ".", "..":
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/")
}

// sarifDoc is the subset of the SARIF 2.1.0 schema this port reads.
type sarifDoc struct {
	Version string     `json:"version"`
	Schema  string     `json:"$schema"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool struct {
		Driver     sarifComponent   `json:"driver"`
		Extensions []sarifComponent `json:"extensions"`
	} `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifComponent struct {
	Rules []struct {
		ID                   string `json:"id"`
		DefaultConfiguration struct {
			Level string `json:"level"`
		} `json:"defaultConfiguration"`
	} `json:"rules"`
}

type sarifResult struct {
	RuleID string `json:"ruleId"`
	Rule   struct {
		ID string `json:"id"`
	} `json:"rule"`
	Level   string `json:"level"`
	Message struct {
		Text     string `json:"text"`
		Markdown string `json:"markdown"`
	} `json:"message"`
	Locations []struct {
		PhysicalLocation struct {
			ArtifactLocation struct {
				URI string `json:"uri"`
			} `json:"artifactLocation"`
			Region struct {
				StartLine int `json:"startLine"`
			} `json:"region"`
		} `json:"physicalLocation"`
	} `json:"locations"`
}

// ParseSARIF reads a SARIF 2.1.0 file and flattens it into findings, one per
// result location (a result with no location still produces one entry, with an
// empty Path — matching Python).
//
// Every failure mode is an error wrapping ErrMalformedSARIF or os.ErrNotExist;
// it never panics on adversarial input.
func ParseSARIF(sarifPath string) ([]Finding, error) {
	data, err := os.ReadFile(sarifPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("SARIF file not found: %s: %w", sarifPath, os.ErrNotExist)
		}
		return nil, err
	}
	return ParseSARIFBytes(data, sarifPath)
}

// ParseSARIFBytes is ParseSARIF over an in-memory document. name is only used
// in error messages.
func ParseSARIFBytes(data []byte, name string) ([]Finding, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrMalformedSARIF, name)
	}
	// Decode into a generic value first so we can tell "not a JSON object" and
	// "object without runs" apart from "SARIF with zero findings".
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%w: cannot parse %s as JSON: %v", ErrMalformedSARIF, name, err)
	}
	obj, ok := probe.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a SARIF log (top level must be a JSON object with a \"runs\" array)", ErrMalformedSARIF, name)
	}
	rawRuns, hasRuns := obj["runs"]
	if !hasRuns {
		return nil, fmt.Errorf("%w: %s has no \"runs\" array (is this really a SARIF log?)", ErrMalformedSARIF, name)
	}
	if _, ok := rawRuns.([]any); !ok {
		return nil, fmt.Errorf("%w: %s has a \"runs\" key that is not an array", ErrMalformedSARIF, name)
	}

	var doc sarifDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s does not match the SARIF schema: %v", ErrMalformedSARIF, name, err)
	}

	findings := []Finding{}
	for _, run := range doc.Runs {
		ruleSeverity := map[string]string{}
		components := append([]sarifComponent{run.Tool.Driver}, run.Tool.Extensions...)
		for _, comp := range components {
			for _, r := range comp.Rules {
				if r.ID == "" {
					continue
				}
				lvl := r.DefaultConfiguration.Level
				if lvl == "" {
					lvl = "warning"
				}
				if _, seen := ruleSeverity[r.ID]; !seen {
					ruleSeverity[r.ID] = lvl
				}
			}
		}
		for _, res := range run.Results {
			ruleID := res.RuleID
			if ruleID == "" {
				ruleID = res.Rule.ID
			}
			if ruleID == "" {
				ruleID = "unknown"
			}
			msg := res.Message.Text
			if msg == "" {
				msg = res.Message.Markdown
			}
			sev := res.Level
			if sev == "" {
				if s, ok := ruleSeverity[ruleID]; ok {
					sev = s
				} else {
					sev = "warning"
				}
			}
			if len(res.Locations) == 0 {
				findings = append(findings, Finding{RuleID: ruleID, Message: msg, Severity: sev})
				continue
			}
			for _, loc := range res.Locations {
				findings = append(findings, Finding{
					RuleID:    ruleID,
					Message:   msg,
					Path:      normalizeSARIFPath(loc.PhysicalLocation.ArtifactLocation.URI),
					StartLine: loc.PhysicalLocation.Region.StartLine,
					Severity:  sev,
				})
			}
		}
	}
	return findings, nil
}

// FilterBySeverity keeps only findings whose severity is in keep. An empty keep
// set is a no-op (everything passes). Unknown names are reported so a typo in
// `--severity critcal` is a usage error rather than a silently empty run — the
// exact "silently drops work but reports success" trap.
func FilterBySeverity(findings []Finding, keep []string) ([]Finding, error) {
	if len(keep) == 0 {
		return findings, nil
	}
	want := map[string]bool{}
	var unknown []string
	for _, k := range keep {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if _, ok := sarifSeverityRank[k]; !ok {
			unknown = append(unknown, k)
			continue
		}
		want[k] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown SARIF severity %v (valid: %s)",
			unknown, strings.Join(KnownSeverities(), ", "))
	}
	if len(want) == 0 {
		return findings, nil
	}
	out := []Finding{}
	for _, f := range findings {
		if want[strings.ToLower(f.Severity)] {
			out = append(out, f)
		}
	}
	return out, nil
}

// SortFindings orders findings by severity (most serious first), then path,
// then line, so output is stable across runs and JSON diffs are meaningful.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, ok := sarifSeverityRank[strings.ToLower(fs[i].Severity)]
		if !ok {
			ri = len(sarifSeverityRank)
		}
		rj, ok := sarifSeverityRank[strings.ToLower(fs[j].Severity)]
		if !ok {
			rj = len(sarifSeverityRank)
		}
		if ri != rj {
			return ri < rj
		}
		if fs[i].Path != fs[j].Path {
			return fs[i].Path < fs[j].Path
		}
		return fs[i].StartLine < fs[j].StartLine
	})
}

// ResolveInRepo joins a finding's repo-relative path against repoRoot and
// refuses anything that escapes it (a crafted SARIF must not be able to steer a
// write to /etc/cron.d/evil). Returns the absolute path.
func ResolveInRepo(repoRoot, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("finding has no file path")
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	target := filepath.Clean(filepath.Join(root, rel))
	// EvalSymlinks on the target's existing prefix, so a symlinked file inside
	// the repo pointing outside it is also rejected.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	relCheck, err := filepath.Rel(root, target)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the repository root %s", rel, root)
	}
	return target, nil
}

// BuildVulnFixPrompt renders the per-finding remediation prompt. Mirrors
// build_vuln_fix_prompt (same sections, same ±10-line window, same 6000-char
// file cap) so output is comparable between the two engines.
func BuildVulnFixPrompt(f Finding, fileContent string) string {
	lines := strings.Split(fileContent, "\n")
	lo := f.StartLine - 10
	if lo < 0 {
		lo = 0
	}
	hi := f.StartLine + 10
	if hi > len(lines) {
		hi = len(lines)
	}
	if lo > hi {
		lo = hi
	}
	var snip strings.Builder
	for i, l := range lines[lo:hi] {
		fmt.Fprintf(&snip, "%d: %s\n", i+1+lo, l)
	}
	body := fileContent
	if len(body) > 6000 {
		body = body[:6000]
	}
	pathLabel := f.Path
	if pathLabel == "" {
		pathLabel = "unknown file"
	}
	return fmt.Sprintf(`## Vulnerability Details

- **Rule**: %s
- **Severity**: %s
- **File**: %s
- **Line**: %d
- **Finding**: %s

## Relevant Code (lines %d-%d)

`+"```"+`
%s`+"```"+`

## Full File Content

`+"```"+`
%s
`+"```"+`

## Task

Provide a corrected version of the FULL file that fixes the vulnerability
described above. Output ONLY the complete corrected file content -
no explanations, no markdown fences.
`, f.RuleID, f.Severity, pathLabel, f.StartLine, f.Message, lo+1, hi, snip.String(), body)
}

// VulnFixSystemPrompt is the system message for fix-vuln.
const VulnFixSystemPrompt = "You are a security engineer performing automated vulnerability remediation. " +
	"Output only the corrected file content."
