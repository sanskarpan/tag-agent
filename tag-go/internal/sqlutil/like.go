// Package sqlutil holds tiny, dependency-free SQL helpers shared across the
// data-access packages. It deliberately imports nothing from the rest of the
// tree so packages that are decoupled from internal/store (internal/benchmark,
// internal/evaljudge) can use it too.
package sqlutil

import "strings"

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike makes s safe to embed in a SQL LIKE pattern, so that user input
// containing %, _ or \ matches those characters LITERALLY instead of acting as
// wildcards. The result must be used with an explicit ESCAPE '\' clause:
//
//	WHERE content LIKE ? ESCAPE '\'          -- bind "%"+EscapeLike(q)+"%"
//	WHERE id LIKE ? || '%' ESCAPE '\'        -- bind EscapeLike(prefix)
//
// Mirrors semantic_memory.py's escaping (issue #567). Without it a search for
// "%" matches every row, and an id PREFIX of "%" or "_" resolves to an
// arbitrary record — which let `context compress --session-id '%'` write a
// context_compressions row against a session the user never named.
func EscapeLike(s string) string { return likeEscaper.Replace(s) }
