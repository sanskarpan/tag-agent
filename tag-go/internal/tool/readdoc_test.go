package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/docs"
)

func hasTool(reg *agent.Registry, name string) bool {
	for _, d := range reg.Defs() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// read_document is registered only when the engine is installed. A tool the
// model can call that always fails teaches it the capability is broken rather
// than absent — and it will keep trying.
func TestReadDocumentRegisteredOnlyWhenAvailable(t *testing.T) {
	t.Setenv(docs.EnvOverride, filepath.Join(t.TempDir(), "absent"))
	reg := agent.NewRegistry()
	opts := DefaultOptions()
	opts.Root = t.TempDir()
	Register(reg, opts)
	if hasTool(reg, "read_document") {
		t.Error("registered with no engine installed")
	}
	if !hasTool(reg, "read_file") {
		t.Fatal("read_file must always be present")
	}

	stub := filepath.Join(t.TempDir(), docs.BinaryName)
	os.WriteFile(stub, []byte("#!/bin/sh\necho '{}'\n"), 0o755)
	t.Setenv(docs.EnvOverride, stub)
	reg2 := agent.NewRegistry()
	Register(reg2, opts)
	if !hasTool(reg2, "read_document") {
		t.Error("not registered with the engine installed")
	}
}

// The read_file refusal must point at what WOULD work, and the advice depends
// on whether the engine is installed — offering advice that does not apply is
// its own dead end.
func TestRefusalAdviceDependsOnAvailability(t *testing.T) {
	t.Setenv(docs.EnvOverride, filepath.Join(t.TempDir(), "absent"))
	if got := readAdvice("spec.pdf"); !strings.Contains(got, "npm install") {
		t.Errorf("with no engine, the advice should be how to install it: %q", got)
	}
	if got := readAdvice("photo.png"); strings.Contains(got, "npm install") {
		t.Errorf("a PNG is not a PDF; the install hint does not apply: %q", got)
	}

	stub := filepath.Join(t.TempDir(), docs.BinaryName)
	os.WriteFile(stub, []byte("#!/bin/sh\necho '{}'\n"), 0o755)
	t.Setenv(docs.EnvOverride, stub)
	if got := readAdvice("spec.pdf"); !strings.Contains(got, "read_document") {
		t.Errorf("with the engine installed, point at the tool: %q", got)
	}
}

// A complete read carries no preamble; an incomplete one leads with the caveat,
// because a warning after 4,000 words is a warning the model will not weigh.
func TestRenderDocumentLeadsWithProblems(t *testing.T) {
	clean := renderDocument(&docs.Document{
		Markdown: "# Title\n\nbody", Complete: true, PageCount: 2,
	}, "a.pdf")
	if !strings.HasPrefix(clean, "# Title") {
		t.Errorf("a clean read must be unadorned, got %.60q", clean)
	}

	partial := renderDocument(&docs.Document{
		Markdown: "# Title\n\nbody", Complete: false, PageCount: 3, Type: "Mixed",
		PagesNeedingOCR: []int{2},
		Notes:           []string{"1 of 3 page(s) produced no text and need OCR: 2"},
	}, "a.pdf")
	if !strings.HasPrefix(partial, "[read_document: a.pdf") {
		t.Errorf("an incomplete read must lead with the header, got %.80q", partial)
	}
	for _, want := range []string{"INCOMPLETE", "do not write it back", "need OCR: 2"} {
		if !strings.Contains(partial, want) {
			t.Errorf("missing %q in:\n%s", want, partial)
		}
	}
	if !strings.Contains(partial, "# Title") {
		t.Error("the content itself must still be returned")
	}
}
