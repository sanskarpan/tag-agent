package permission

import (
	"context"
	"errors"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
)

func dummyTool(name string, ran *bool) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{Name: name},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			*ran = true
			return "did the thing", nil
		},
	}
}

// TestWrapReturnsDeniedError pins the error type and that Exec never runs.
func TestWrapReturnsDeniedError(t *testing.T) {
	ran := false
	tl := Wrap(headless(DefaultPolicy()), dummyTool(ToolBash, &ran),
		func(in map[string]any) (Kind, string) { return KindCommand, "rm -rf /" })
	out, err := tl.Exec(context.Background(), map[string]any{"command": "rm -rf /"})
	if err == nil {
		t.Fatal("expected a denial")
	}
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("want *DeniedError, got %T", err)
	}
	if de.Decision.Action != Deny || de.Request.Tool != ToolBash {
		t.Errorf("denied error does not carry the decision: %+v", de)
	}
	if out != "" {
		t.Errorf("a denied call must return no output, got %q", out)
	}
	if ran {
		t.Fatal("the tool body executed despite the denial")
	}
}

// TestWrapAllowsAndPassesThrough
func TestWrapAllowsAndPassesThrough(t *testing.T) {
	ran := false
	tl := Wrap(UnsafeAllowAllGuard(), dummyTool(ToolBash, &ran), NoSubject)
	out, err := tl.Exec(context.Background(), map[string]any{"command": "ls"})
	if err != nil || out != "did the thing" || !ran {
		t.Fatalf("allowed call should pass through: %q %v ran=%v", out, err, ran)
	}
}

// TestWrapWithNilGuardFailsClosed — a wiring bug denies rather than ungates.
func TestWrapWithNilGuardFailsClosed(t *testing.T) {
	ran := false
	tl := Wrap(nil, dummyTool(ToolBash, &ran), NoSubject)
	if _, err := tl.Exec(context.Background(), map[string]any{"command": "ls"}); err == nil {
		t.Fatal("a nil guard must deny")
	}
	if ran {
		t.Fatal("the tool body executed with no guard")
	}
}

// TestBlanketAllowDoesNotCoverCredentialPaths is the property that makes
// `--allow-tool read_file` safe to type.
func TestBlanketAllowDoesNotCoverCredentialPaths(t *testing.T) {
	blanket, err := ParseSpec("read_file", Allow)
	if err != nil {
		t.Fatal(err)
	}
	g := headless(Policy{Rules: Resolve(Sources{Flags: []Rule{blanket}})})
	if d := g.Check(context.Background(), pathReq(ToolReadFile, "/repo/.env")); d.Allowed() {
		t.Errorf("a pattern-less allow must not cover .env: %+v", d)
	}
	if d := g.Check(context.Background(), pathReq(ToolReadFile, "/repo/src/main.go")); !d.Allowed() {
		t.Errorf("it must still allow ordinary files: %+v", d)
	}
	// naming the path explicitly DOES cover it (deliberate, visible opt-in)
	named, err := ParseSpec("read_file:*.env", Allow)
	if err != nil {
		t.Fatal(err)
	}
	g2 := headless(Policy{Rules: Resolve(Sources{Flags: []Rule{named}})})
	if d := g2.Check(context.Background(), pathReq(ToolReadFile, "/repo/.env")); !d.Allowed() {
		t.Errorf("an explicit patterned allow should cover .env: %+v", d)
	}
	// an explicit DENY is never skipped, even for the .env.example carve-out
	denyAll, err := ParseSpec("read_file", Deny)
	if err != nil {
		t.Fatal(err)
	}
	g3 := headless(Policy{Rules: Resolve(Sources{Flags: []Rule{denyAll}})})
	if d := g3.Check(context.Background(), pathReq(ToolReadFile, "/repo/.env.example")); d.Allowed() {
		t.Errorf("an explicit deny must still win over the carve-out: %+v", d)
	}
}

func TestIsCredentialPath(t *testing.T) {
	cases := map[string]bool{
		"/repo/.env":         true,
		"/repo/.env.local":   true,
		"/repo/.env.example": false,
		"/repo/main.go":      false,
		"/repo/tls.key":      true,
	}
	for p, want := range cases {
		if got := IsCredentialPath(pathReq(ToolReadFile, p)); got != want {
			t.Errorf("IsCredentialPath(%q) = %v, want %v", p, got, want)
		}
	}
	if IsCredentialPath(cmdReq("cat .env")) {
		t.Error("a command is never a credential path")
	}
}
