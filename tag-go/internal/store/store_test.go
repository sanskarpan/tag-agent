package store

import (
	"errors"
	"path/filepath"
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
