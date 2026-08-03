package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/docs"
	"github.com/tag-agent/tag/internal/llm"
)

// readDocumentTool converts a document into text the model can actually use.
//
// It exists because read_file now refuses binary: that refusal closed a hole
// (a PDF used to arrive as ~100k tokens of deflate output labelled success) but
// left the underlying request — "read this spec" — with no answer at all. This
// is the answer.
//
// Registered ONLY when the engine is installed. A tool the model can call and
// that always fails is worse than a tool that is not there: the model will keep
// trying it, and the failure looks like a capability problem rather than a
// missing dependency. The install hint lives in the read_file refusal instead,
// where a human sees it.
func readDocumentTool(opts Options) agent.Tool {
	return agent.Tool{
		Def: llm.ToolDef{
			Name: "read_document",
			Description: "Read a PDF as text (confined to the tool root). Returns markdown plus " +
				"which pages, if any, could not be read and need OCR. Use this for documents; " +
				"read_file is for text files.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
		Exec: func(ctx context.Context, in map[string]any) (string, error) {
			p, err := resolvePath(opts, "read_document", strArg(in, "path"))
			if err != nil {
				return "", err
			}
			doc, err := docs.Extract(ctx, p, docs.Options{MaxBytes: int(opts.MaxReadBytes)})
			if err != nil {
				return "", err
			}
			return renderDocument(doc, strArg(in, "path")), nil
		},
	}
}

// renderDocument formats an extraction for the model.
//
// Anything short of "the whole document as text" is stated FIRST and in the
// imperative, for the same reason the truncation notice leads: the model acts on
// what it reads, and a caveat after 4,000 words of content is a caveat it will
// not weigh. A complete extraction gets no preamble at all — a warning on every
// read teaches the model to ignore warnings.
func renderDocument(doc *docs.Document, displayPath string) string {
	var b strings.Builder
	if !doc.Complete || len(doc.Notes) > 0 {
		fmt.Fprintf(&b, "[read_document: %s", displayPath)
		if doc.PageCount > 0 {
			fmt.Fprintf(&b, ", %d page(s), %s", doc.PageCount, doc.Type)
		}
		b.WriteString("]\n")
		if !doc.Complete {
			b.WriteString("[INCOMPLETE — the text below is not the whole document. " +
				"Do not treat it as complete, and do not write it back to any file.]\n")
		}
		for _, n := range doc.Notes {
			b.WriteString("[note: " + n + "]\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(doc.Markdown)
	return b.String()
}
