package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// validValueTypes mirrors Python's _VALID_VALUE_TYPES.
var validValueTypes = map[string]bool{
	"string": true, "number": true, "boolean": true,
	"json_object": true, "json_array": true,
}

// ContextBus is the write-once, per-agent-permissioned key/value bus backed by
// the swarm_context table (port of Python's ContextBus).
//
// The Python bus is used from a ThreadPoolExecutor over one sqlite3 connection;
// Go runs sub-agents on real goroutines against a single-writer *sql.DB, so the
// bus carries its own mutex. Serialising bus writes also makes the write-once
// rule race-free, which the Python version only gets by accident of the GIL.
type ContextBus struct {
	mu      sync.Mutex
	db      *sql.DB
	swarmID string
}

// NewContextBus builds a bus for one swarm run.
func NewContextBus(db *sql.DB, swarmID string) *ContextBus {
	return &ContextBus{db: db, swarmID: swarmID}
}

// BusEntry is one audited context-bus record.
type BusEntry struct {
	Key       string `json:"key"`
	Value     any    `json:"value"`
	ValueType string `json:"value_type"`
	WrittenBy string `json:"written_by"`
	WrittenAt string `json:"written_at"`
}

// Write stores value under key on behalf of writtenBy.
//
// It returns false (never an error) for every rejection, exactly like Python:
// a sub-agent writing outside its permitted keys is a policy violation to be
// silently dropped, not a run-killing exception.
func (b *ContextBus) Write(ctx context.Context, key string, value any, valueType, writtenBy string, permitted []string) bool {
	if b == nil {
		return false
	}
	if !contains(permitted, key) {
		return false
	}
	if !validValueTypes[valueType] {
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return false
	}
	if !typeMatches(valueType, decoded) {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Write-once: a key belongs to its first writer. The same writer may update
	// its own key; anyone else is rejected.
	var existing string
	err = b.db.QueryRowContext(ctx, `SELECT written_by FROM swarm_context WHERE swarm_id=? AND key=?`,
		b.swarmID, key).Scan(&existing)
	if err == nil && existing != writtenBy {
		return false
	}
	if err != nil && err != sql.ErrNoRows {
		return false
	}
	_, err = b.db.ExecContext(ctx,
		`INSERT INTO swarm_context(swarm_id, key, value, value_type, written_by, written_at, schema_hint)
		 VALUES(?,?,?,?,?,?,NULL)
		 ON CONFLICT(swarm_id, key) DO UPDATE SET value=excluded.value, written_at=excluded.written_at
		 WHERE written_by=excluded.written_by`,
		b.swarmID, key, string(encoded), valueType, writtenBy, nowISO())
	return err == nil
}

// ReadSnapshot returns the entries for the keys this task may read.
func (b *ContextBus) ReadSnapshot(ctx context.Context, permitted []string) map[string]BusEntry {
	out := map[string]BusEntry{}
	if b == nil || len(permitted) == 0 {
		return out
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ph := strings.TrimSuffix(strings.Repeat("?,", len(permitted)), ",")
	args := make([]any, 0, len(permitted)+1)
	args = append(args, b.swarmID)
	for _, k := range permitted {
		args = append(args, k)
	}
	rows, err := b.db.QueryContext(ctx,
		`SELECT key, value, value_type, written_by FROM swarm_context WHERE swarm_id=? AND key IN (`+ph+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v, vt, by string
		if err := rows.Scan(&k, &v, &vt, &by); err != nil {
			return out
		}
		var decoded any
		_ = json.Unmarshal([]byte(v), &decoded)
		out[k] = BusEntry{Key: k, Value: decoded, ValueType: vt, WrittenBy: by}
	}
	return out
}

// FullAudit returns every bus entry for the run, oldest first.
func (b *ContextBus) FullAudit(ctx context.Context) []BusEntry {
	entries := []BusEntry{}
	if b == nil {
		return entries
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rows, err := b.db.QueryContext(ctx,
		`SELECT key, value, value_type, written_by, written_at FROM swarm_context WHERE swarm_id=? ORDER BY written_at, id`,
		b.swarmID)
	if err != nil {
		return entries
	}
	defer rows.Close()
	for rows.Next() {
		var e BusEntry
		var v string
		if err := rows.Scan(&e.Key, &v, &e.ValueType, &e.WrittenBy, &e.WrittenAt); err != nil {
			return entries
		}
		_ = json.Unmarshal([]byte(v), &e.Value)
		entries = append(entries, e)
	}
	return entries
}

// inferValueType maps a Go value onto the bus's declared type vocabulary,
// mirroring the isinstance ladder Python applies to a sub-agent's ctx_out.json.
func inferValueType(v any) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, float32, int, int64, int32:
		return "number"
	case map[string]any:
		return "json_object"
	case []any:
		return "json_array"
	default:
		return "string"
	}
}

func typeMatches(valueType string, decoded any) bool {
	switch valueType {
	case "string":
		_, ok := decoded.(string)
		return ok
	case "number":
		// json.Unmarshal into `any` yields float64 for every JSON number, and
		// Python's isinstance(x, (int, float)) explicitly excludes bool.
		if _, isBool := decoded.(bool); isBool {
			return false
		}
		_, ok := decoded.(float64)
		return ok
	case "boolean":
		_, ok := decoded.(bool)
		return ok
	case "json_object":
		_, ok := decoded.(map[string]any)
		return ok
	case "json_array":
		_, ok := decoded.([]any)
		return ok
	}
	return false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// sortedCopy returns list sorted, without mutating the caller's slice.
func sortedCopy(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}
