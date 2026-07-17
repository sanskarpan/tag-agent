// Package store owns the single SQLite state store (Go port of core/db.py).
// Pure-Go driver (modernc.org/sqlite), FTS5 compiled in, CGO-free.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/paths"
	_ "modernc.org/sqlite"
)

//go:embed migrate/schema.sql
var schemaSQL string

// DB wraps the single writer connection.
type DB struct {
	*sql.DB
	Path string
}

// Open opens (and migrates) the runtime DB derived from cfg.
func Open(cfg *config.Config) (*DB, error) {
	rt := cfg.Section("runtime")
	homeDir, _ := rt["home_dir"].(string)
	dbPath, _ := rt["db_path"].(string)
	if err := paths.EnsureRuntimeDirs(homeDir, dbPath); err != nil {
		return nil, err
	}
	return OpenPath(paths.RuntimeDBPath(dbPath))
}

// OpenPath opens (and migrates) a DB at an explicit path.
func OpenPath(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", (&url.URL{Path: path}).EscapedPath())
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// single-writer discipline: serialize writes on one conn
	sqldb.SetMaxOpenConns(1)
	// The initial DDL and the WAL journal-mode switch take a short exclusive
	// lock that busy_timeout does not always honor, so a fresh cold-start race
	// between separate `tag` processes can return SQLITE_BUSY immediately.
	// Retry the migration with backoff so concurrent first-runs don't lose
	// their writes.
	if err := migrateWithRetry(sqldb); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &DB{DB: sqldb, Path: path}, nil
}

// migrateWithRetry applies the embedded schema, retrying on a transient
// SQLITE_BUSY / "database is locked" that a concurrent cold-start can trigger.
func migrateWithRetry(sqldb *sql.DB) error {
	const attempts = 12
	delay := 25 * time.Millisecond
	var err error
	for i := 0; i < attempts; i++ {
		if _, err = sqldb.Exec(schemaSQL); err == nil {
			return nil
		}
		if !isBusy(err) {
			return err
		}
		time.Sleep(delay)
		if delay < 500*time.Millisecond {
			delay *= 2
		}
	}
	return err
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "(5)")
}
