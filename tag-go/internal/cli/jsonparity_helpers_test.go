package cli_test

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// runIO is like run but keeps stdout and stderr apart. Several --json parity
// findings are specifically about WHICH stream a payload lands on (a success-
// shaped object printed to stdout on a failure path is worse than no output at
// all), and CombinedOutput cannot see that.
func runIO(t *testing.T, home string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return so.String(), se.String(), code
}

// runtimeDB opens the runtime SQLite file inside an isolated TAG_HOME so a test
// can fabricate state the CLI itself refuses to create.
func runtimeDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	path := filepath.Join(home, "runtime", "tag.sqlite3")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("runtime db not found at %s: %v", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open runtime db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedDuplicateCronRows writes two cron_jobs rows sharing a name, reproducing a
// TAG_HOME created by a build that predates the uniqueness check.
func seedDuplicateCronRows(t *testing.T, home string) {
	t.Helper()
	// Make sure the schema exists before we poke at it.
	if out, code := run(t, home, "--json", "cron", "list"); code != 0 {
		t.Fatalf("cron list (schema warmup): exit %d: %s", code, out)
	}
	db := runtimeDB(t, home)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range []string{"legacy01", "legacy02"} {
		if _, err := db.Exec(
			`INSERT INTO cron_jobs(id,name,schedule,task,profile,enabled,created_at,run_count)
			 VALUES(?,?,?,?,?,1,?,0)`,
			id, "legacydup", "* * * * *", "legacy task", "orchestrator", now,
		); err != nil {
			t.Fatalf("seed duplicate cron row %s: %v", id, err)
		}
	}
}
