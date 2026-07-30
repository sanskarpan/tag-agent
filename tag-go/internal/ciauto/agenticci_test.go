package ciauto

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- SARIF ------------------------------------------------------------------

const sampleSARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"bandit","rules":[
 {"id":"B605","defaultConfiguration":{"level":"error"}},
 {"id":"B101","defaultConfiguration":{"level":"note"}}]}},
"results":[
 {"ruleId":"B605","message":{"text":"shell=True"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"pkg/run.py"},"region":{"startLine":12}}}]},
 {"ruleId":"B101","message":{"text":"assert used"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"file:///%2e%2e/%2e%2e/etc/passwd"},"region":{"startLine":1}}}]},
 {"ruleId":"B999","level":"warning","message":{"markdown":"*md only*"}}
]}]}`

func TestParseSARIF(t *testing.T) {
	fs, err := ParseSARIFBytes([]byte(sampleSARIF), "s.sarif")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(fs) != 3 {
		t.Fatalf("want 3 findings, got %d: %+v", len(fs), fs)
	}
	if fs[0].RuleID != "B605" || fs[0].Path != "pkg/run.py" || fs[0].StartLine != 12 || fs[0].Severity != "error" {
		t.Errorf("finding 0 wrong: %+v", fs[0])
	}
	// Severity inherited from the rule's defaultConfiguration when the result
	// carries no explicit level.
	if fs[1].Severity != "note" {
		t.Errorf("want severity note from rule default, got %q", fs[1].Severity)
	}
	// Traversal must be stripped: the URI decodes to ../../etc/passwd.
	if strings.Contains(fs[1].Path, "..") || !strings.HasSuffix(fs[1].Path, "etc/passwd") {
		t.Errorf("traversal not normalised: %q", fs[1].Path)
	}
	// A result with no locations still yields one entry, and markdown is the
	// message fallback.
	if fs[2].Path != "" || fs[2].Message != "*md only*" {
		t.Errorf("location-less result wrong: %+v", fs[2])
	}
}

func TestParseSARIFMalformedIsAnError(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"not json":      "not json at all",
		"json array":    "[]",
		"json scalar":   `"hello"`,
		"no runs key":   `{"version":"2.1.0"}`,
		"runs not list": `{"runs":{"a":1}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			// Must be an ErrMalformedSARIF error, and must NOT panic.
			fs, err := ParseSARIFBytes([]byte(body), "x.sarif")
			if err == nil {
				t.Fatalf("want error, got %d findings", len(fs))
			}
			if !strings.Contains(err.Error(), "malformed SARIF") {
				t.Fatalf("want malformed-SARIF error, got %v", err)
			}
		})
	}
}

func TestParseSARIFMissingFileWrapsNotExist(t *testing.T) {
	_, err := ParseSARIF(filepath.Join(t.TempDir(), "nope.sarif"))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want an os.ErrNotExist-wrapping error, got %v", err)
	}
}

func TestFilterBySeverityRejectsTypos(t *testing.T) {
	fs, _ := ParseSARIFBytes([]byte(sampleSARIF), "s.sarif")
	if _, err := FilterBySeverity(fs, []string{"critcal"}); err == nil {
		t.Fatal("a misspelled severity must be an error, not a silently empty selection")
	}
	kept, err := FilterBySeverity(fs, []string{"error"})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].RuleID != "B605" {
		t.Fatalf("severity filter wrong: %+v", kept)
	}
	all, err := FilterBySeverity(fs, nil)
	if err != nil || len(all) != len(fs) {
		t.Fatalf("empty filter must be a no-op, got %d/%v", len(all), err)
	}
}

func TestResolveInRepoRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolveInRepo(root, "etc/passwd"); err != nil {
		t.Fatalf("in-repo path must resolve: %v", err)
	}
	// normalizeSARIFPath already strips "..", but ResolveInRepo is the last line
	// of defence and must reject one that slips through.
	if _, err := ResolveInRepo(root, "../../../etc/passwd"); err == nil {
		t.Fatal("escaping path must be rejected")
	}
	if _, err := ResolveInRepo(root, ""); err == nil {
		t.Fatal("empty path must be rejected")
	}
}

// --- CI log -----------------------------------------------------------------

func TestParseCIFailurePytest(t *testing.T) {
	log := `collecting ...
  File "app/svc.py", line 42, in handle
    return payload["id"]
KeyError: 'id'
FAILED tests/test_svc.py::test_handle - KeyError: 'id'
1 failed`
	f := ParseCIFailure(log)
	if f.FilePath != "app/svc.py" || f.LineNumber != 42 {
		t.Errorf("file/line wrong: %+v", f)
	}
	if f.TestName != "tests/test_svc.py::test_handle" {
		t.Errorf("test name wrong: %q", f.TestName)
	}
	if f.ErrorType != "KeyError" {
		t.Errorf("error type wrong: %q", f.ErrorType)
	}
	if f.Empty() {
		t.Error("parsed failure must not report Empty()")
	}
}

func TestParseCIFailureGoAndUnrecognised(t *testing.T) {
	f := ParseCIFailure("--- FAIL: TestThing (0.02s)\nFAIL\texample.com/pkg\t0.1s")
	if f.TestName != "TestThing" {
		t.Errorf("go test name wrong: %+v", f)
	}
	// An unrecognised log must report Empty() so the CLI can say "no pattern
	// matched" instead of implying a confident diagnosis.
	if !ParseCIFailure("everything is fine\n").Empty() {
		t.Error("unrecognised log must be Empty()")
	}
}

func TestReadCILogRejectsBlank(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ci.log")
	if err := os.WriteFile(p, []byte("   \n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCILog(p); err == nil || !strings.Contains(err.Error(), "empty CI log") {
		t.Fatalf("want ErrEmptyLog, got %v", err)
	}
}

func TestBuildDiagnosePromptKeepsTheTail(t *testing.T) {
	// The failure is at the END of a CI log; truncating the head must keep it.
	log := strings.Repeat("noise\n", 4000) + "KeyError: 'needle'\n"
	p := BuildDiagnosePrompt(ParseCIFailure(log), log)
	if !strings.Contains(p, "needle") {
		t.Fatal("prompt dropped the tail of the log, which is where the failure lives")
	}
	if !strings.Contains(p, "truncated") {
		t.Fatal("truncation must be disclosed in the prompt")
	}
}

// --- flaky ------------------------------------------------------------------

const flakyLog = `PASSED tests/a.py::test_x
FAILED tests/b.py::test_y
--- PASS: TestZ (0.01s)
FAILED tests/a.py::test_x
FAILED tests/b.py::test_y
--- FAIL: TestZ (0.01s)
PASSED tests/a.py::test_x
FAILED tests/b.py::test_y
--- PASS: TestZ (0.01s)
`

func TestDetectFlaky(t *testing.T) {
	got := DetectFlaky(flakyLog, 0)
	if len(got) != 2 {
		t.Fatalf("want 2 flaky tests (test_y never passes, so it is broken not flaky), got %d: %+v", len(got), got)
	}
	byName := map[string]FlakyTest{}
	for _, f := range got {
		byName[f.TestName] = f
	}
	x, ok := byName["tests/a.py::test_x"]
	if !ok {
		t.Fatalf("pytest test missing: %+v", got)
	}
	if x.PassCount != 2 || x.FailCount != 1 {
		t.Errorf("counts wrong: %+v", x)
	}
	if _, ok := byName["TestZ"]; !ok {
		t.Errorf("go test missing: %+v", got)
	}
	if _, ok := byName["tests/b.py::test_y"]; ok {
		t.Error("an always-failing test is NOT flaky and must not be reported")
	}
}

func TestDetectFlakyQuarantineThreshold(t *testing.T) {
	// 1 pass / 3 fails = 0.75 fail rate.
	log := "PASSED t.py::a\nFAILED t.py::a\nFAILED t.py::a\nFAILED t.py::a\n"
	if got := DetectFlaky(log, 0.5); len(got) != 1 || !got[0].Quarantine {
		t.Fatalf("want quarantine=true at 0.75 >= 0.5: %+v", got)
	}
	if got := DetectFlaky(log, 0.9); len(got) != 1 || got[0].Quarantine {
		t.Fatalf("want quarantine=false at 0.75 < 0.9: %+v", got)
	}
}

func TestDetectFlakyEmptyIsEmptySliceNotNil(t *testing.T) {
	got := DetectFlaky("nothing here", 0)
	if got == nil {
		t.Fatal("DetectFlaky must return an empty slice, not nil (JSON contract: [] not null)")
	}
	if len(got) != 0 {
		t.Fatalf("want 0, got %+v", got)
	}
}

func TestSplitFlakyFixRefusesUnparseableOutput(t *testing.T) {
	code, expl, ok := SplitFlakyFix("def test_a():\n    pass\nEXPLANATION: seeded the RNG")
	if !ok || expl != "seeded the RNG" || !strings.Contains(code, "def test_a") {
		t.Fatalf("split wrong: %q / %q / %v", code, expl, ok)
	}
	// No marker -> ok=false, so the caller must NOT overwrite the test file.
	if _, _, ok := SplitFlakyFix("I think the test is flaky because of timing."); ok {
		t.Fatal("output without the EXPLANATION marker must be reported as unparseable")
	}
}

func TestResolveTestFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	pyt := filepath.Join(root, "tests", "test_a.py")
	if err := os.WriteFile(pyt, []byte("def test_x(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ResolveTestFile(root, "tests/test_a.py::test_x")
	if got == "" || filepath.Base(got) != "test_a.py" {
		t.Fatalf("pytest resolution failed: %q", got)
	}
	got = ResolveTestFile(root, "tests/test_a.py::test_missing")
	if got == "" {
		t.Fatal("pytest resolution keys off the file part, so it should still resolve")
	}
	if ResolveTestFile(root, "TotallyUnknownTest") != "" {
		t.Fatal("unresolvable name must return \"\" so the caller can report it honestly")
	}
}

// --- signals ----------------------------------------------------------------

func TestNormalizeSignals(t *testing.T) {
	all, err := NormalizeSignals(nil)
	if err != nil || len(all) != len(SignalClasses) {
		t.Fatalf("empty request must select every class: %v / %v", all, err)
	}
	got, err := NormalizeSignals([]string{"security,style", "security"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "security" || got[1] != "style" {
		t.Fatalf("want deduped [security style], got %v", got)
	}
	if _, err := NormalizeSignals([]string{"bogus"}); err == nil {
		t.Fatal("unknown signal class must be an error, not silently dropped")
	}
}

// TestDetectSignalFindingsDoesNotCountMereMentions is the regression test for
// the Python bug: review_pr_with_signals reported a class as "found" whenever
// its NAME appeared in the review text -- but the prompt makes the model name
// every class, so a spotless review reported findings in all six.
func TestDetectSignalFindingsDoesNotCountMereMentions(t *testing.T) {
	signals := []string{"security", "style"}

	clean := "security: CLEAN\nstyle: CLEAN\nOverall: APPROVE"
	found, parsed := DetectSignalFindings(clean, signals)
	if !parsed {
		t.Fatal("verdict lines present but not parsed")
	}
	if len(found) != 0 {
		t.Fatalf("a clean review must report NO findings, got %v", found)
	}

	mixed := "- security: FINDINGS\n  - injection at x.py:3\n- style: CLEAN\n"
	found, parsed = DetectSignalFindings(mixed, signals)
	if !parsed || len(found) != 1 || found[0] != "security" {
		t.Fatalf("want [security], got %v (parsed=%v)", found, parsed)
	}

	// Prose that merely mentions both class names must NOT count as findings,
	// and must be reported as unparseable rather than as a clean bill of health.
	prose := "I looked at security and style and everything seemed fine."
	found, parsed = DetectSignalFindings(prose, signals)
	if len(found) != 0 {
		t.Fatalf("mere mentions must not count as findings, got %v", found)
	}
	if parsed {
		t.Fatal("prose with no verdict lines must report parsed=false (inconclusive)")
	}
}

func TestDetectSignalFindingsEmptySliceNotNil(t *testing.T) {
	found, _ := DetectSignalFindings("", []string{"security"})
	if found == nil {
		t.Fatal("must return [] not nil for the JSON contract")
	}
}

// --- pipeline ---------------------------------------------------------------

func TestDetectStackAndPipeline(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"go.mod", "Dockerfile"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stack := DetectStack(root)
	if len(stack) != 2 || stack[0] != "go" || stack[1] != "docker" {
		t.Fatalf("want [go docker], got %v", stack)
	}
	y := GenerateGitLabPipeline(stack, PipelineOptions{})
	for _, want := range []string{"stages:", "  - build", "go-test:", "docker-build:"} {
		if !strings.Contains(y, want) {
			t.Errorf("pipeline missing %q:\n%s", want, y)
		}
	}
	// Python emitted every script line with `|| true`, producing a pipeline that
	// could never fail. Refusing to copy that is the point of this assertion.
	if strings.Contains(y, "|| true") {
		t.Error("generated pipeline must not neuter its own jobs with `|| true`")
	}
	if strings.Contains(y, "\nonly:") {
		t.Error("deprecated `only:` must not be emitted; use rules:")
	}
}

func TestGenerateGitLabPipelineEmptyStackIsHonest(t *testing.T) {
	y := GenerateGitLabPipeline(nil, PipelineOptions{})
	if !strings.Contains(y, "No known stack detected") || !strings.Contains(y, "placeholder:") {
		t.Fatalf("empty stack must produce a labelled placeholder, got:\n%s", y)
	}
}

func TestGenerateGitLabPipelineDeploy(t *testing.T) {
	y := GenerateGitLabPipeline([]string{"go"}, PipelineOptions{IncludeDeploy: true})
	if !strings.Contains(y, "deploy-staging:") || !strings.Contains(y, "when: manual") {
		t.Fatalf("deploy job missing or not manual:\n%s", y)
	}
}

func TestDetectTestFramework(t *testing.T) {
	root := t.TempDir()
	if got := DetectTestFramework(root); got != "pytest" {
		t.Errorf("empty dir should fall back to pytest, got %q", got)
	}
	// pyproject.toml without pytest must NOT be claimed as pytest via that marker.
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname='x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectTestFramework(root); got != "go-test" {
		t.Errorf("want go-test (pyproject has no pytest), got %q", got)
	}

	jsroot := t.TempDir()
	if err := os.WriteFile(filepath.Join(jsroot, "package.json"),
		[]byte(`{"devDependencies":{"jest":"^29"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectTestFramework(jsroot); got != "jest" {
		t.Errorf("want jest, got %q", got)
	}
}

// --- work dir ---------------------------------------------------------------

func TestWorkDirIsolatedAndTraversalSafe(t *testing.T) {
	root := t.TempDir()
	a, cleanA, err := WorkDir(root, "job-a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := WorkDir(root, "job-b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two ids must get two distinct dirs")
	}
	esc, _, err := WorkDir(root, "../../etc/pwn")
	if err != nil {
		t.Fatal(err)
	}
	if rel, _ := filepath.Rel(root, esc); rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("id must not steer the dir outside the work root: %s", esc)
	}
	// cleanup removes an empty dir but keeps one with artifacts.
	cleanA()
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Error("empty work dir should be cleaned up")
	}
	c, cleanC, _ := WorkDir(root, "job-c")
	if err := os.WriteFile(filepath.Join(c, "out.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanC()
	if _, err := os.Stat(c); err != nil {
		t.Error("work dir with artifacts must be preserved")
	}
}
