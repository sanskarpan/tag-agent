package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// reconcileColumns brings an EXISTING database forward to the embedded schema.
//
// migrate/schema.sql is written with CREATE TABLE IF NOT EXISTS, which skips the
// whole table when it already exists — so every column added to the schema after
// a given TAG_HOME was created is silently missing from it. The failure is not
// subtle when it lands:
//
//	error: recording run: SQL logic error: table runs has no column named duration_ms
//
// and it is total for the affected command. Fresh installs and CI never see it,
// which is exactly why it survived: the only way to hit it is to have used TAG
// before. It is also cross-distribution — the Python CLI and the Go harness share
// one TAG_HOME, so "ran the Python one first" is the ordinary case.
//
// For each table in the embedded schema this reads PRAGMA table_info and issues
// ALTER TABLE ... ADD COLUMN for anything absent. SQLite's ADD COLUMN is cheap
// (metadata-only) and safe for nullable or defaulted columns, which is what every
// additive column in this schema is.
//
// It fails LOUDLY. A column that cannot be added leaves a half-migrated store,
// and a half-migrated store that opens successfully is worse than one that
// refuses to: the next write fails somewhere further from the cause.
//
// LIMIT: this reconciles MISSING columns only. It does not change an existing
// column's type, nullability or DEFAULT, because SQLite cannot alter those in
// place — the only remedy is a create-copy-drop-rename rebuild, which is a much
// larger risk to take automatically on someone's store at startup. The practical
// consequence is that writers must not depend on a column DEFAULT to supply a
// NOT NULL value: an older table may declare the column without the default. The
// `runs` inserts pass metadata_json explicitly for exactly this reason.
func reconcileColumns(db *sql.DB) error {
	for _, t := range parseSchemaTables(schemaSQL) {
		have, err := existingColumns(db, t.name)
		if err != nil {
			return err
		}
		if len(have) == 0 {
			// The table did not exist, so CREATE TABLE just made it from the current
			// schema. Nothing to reconcile.
			continue
		}
		for _, c := range t.columns {
			if have[strings.ToLower(c.name)] {
				continue
			}
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", t.name, c.def)); err != nil {
				return fmt.Errorf("adding missing column %s.%s: %w", t.name, c.name, err)
			}
		}
	}
	return nil
}

func existingColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("inspecting %s: %w", table, err)
		}
		out[strings.ToLower(name)] = true
	}
	return out, rows.Err()
}

type schemaTable struct {
	name    string
	columns []schemaColumn
}

type schemaColumn struct {
	name string
	def  string // the full column definition, reused verbatim by ADD COLUMN
}

var reCreateTableHead = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// parseSchemaTables extracts table columns from the embedded DDL.
//
// This is a narrow parser for a file we own, not a general SQL parser: it handles
// the shapes migrate/schema.sql actually uses. Anything it cannot confidently
// read as a plain column (a table-level constraint, a parenthesised type) is
// SKIPPED rather than guessed at — a missed reconciliation surfaces as the
// original error, while a wrong ALTER corrupts the store.
func parseSchemaTables(ddl string) []schemaTable {
	var out []schemaTable
	for _, stmt := range splitStatements(stripSQLComments(ddl)) {
		m := reCreateTableHead.FindStringSubmatchIndex(stmt)
		if m == nil {
			continue
		}
		// The body runs from the opening paren the head matched to the LAST
		// closing paren. A non-greedy match to the first ")" would truncate the
		// column list at the first parenthesised constraint — UNIQUE(a, b) — and
		// hand "UNIQUE(a" to ALTER TABLE.
		open := m[1] - 1
		closeIdx := strings.LastIndex(stmt, ")")
		if closeIdx <= open {
			continue
		}
		t := schemaTable{name: stmt[m[2]:m[3]]}
		for _, part := range splitTopLevel(stmt[open+1 : closeIdx]) {
			def := strings.TrimSpace(part)
			if def == "" {
				continue
			}
			// The leading token ends at whitespace OR at "(" — `UNIQUE(profile, key)`
			// has no space, so splitting on whitespace alone yields "UNIQUE(profile,"
			// and the constraint sails past the keyword check as a column name.
			name := leadingToken(def)
			switch strings.ToUpper(name) {
			case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "CONSTRAINT":
				continue // table-level constraint, not a column
			}
			// A generated/defaulted column is fine to ADD; a NOT NULL column with no
			// DEFAULT is not addable to a populated table, and SQLite will say so.
			t.columns = append(t.columns, schemaColumn{name: name, def: def})
		}
		if len(t.columns) > 0 {
			out = append(out, t)
		}
	}
	return out
}

// leadingToken returns the identifier or keyword a column definition starts
// with, stopping at whitespace or an opening paren.
func leadingToken(def string) string {
	i := strings.IndexFunc(def, func(r rune) bool {
		return r == '(' || r == ' ' || r == '\t' || r == '\n'
	})
	if i < 0 {
		return def
	}
	return def[:i]
}

// stripSQLComments removes `-- ...` line comments. They otherwise sit in front
// of a CREATE statement and defeat the anchored head match, which would silently
// skip that table -- the quiet half-coverage this function exists to prevent.
func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// splitStatements splits DDL on semicolons that are not inside parentheses or a
// quoted string, so each CREATE statement can be handled whole.
func splitStatements(s string) []string {
	var out []string
	depth, start := 0, 0
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ';' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// splitTopLevel splits a column list on commas that are not inside parentheses
// or a quoted string.
func splitTopLevel(s string) []string {
	var parts []string
	depth, start := 0, 0
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}
