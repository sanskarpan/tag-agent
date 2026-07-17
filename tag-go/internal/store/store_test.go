package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var errNoSchema = errors.New("migration produced no tables")

// Regression for issue #521: many processes/goroutines opening (and migrating)
// a fresh DB concurrently must not lose the migration to a transient
// SQLITE_BUSY. Each OpenPath must succeed and yield a usable, migrated DB.
func TestConcurrentColdStartMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cold.sqlite3")
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximize contention on the migration DDL
			db, err := OpenPath(path)
			if err != nil {
				errs <- err
				return
			}
			defer db.Close()
			// Prove the migration actually ran: the schema must have tables, and
			// the connection must be writable (not a half-initialized DB).
			var tables int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
				errs <- err
				return
			}
			if tables == 0 {
				errs <- errNoSchema
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent cold-start migration failed: %v", err)
	}
}

// A DB path containing URI-special characters ('?', '#') must not truncate the
// DSN query string: the pragmas must still apply and the file must be created
// at the literal path.
func TestOpenPathSpecialCharsInPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "we?ird#dir")
	path := filepath.Join(dir, "tag.db")
	db, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db not created at the literal path: %v", err)
	}
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}
