package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/tag-agent/tag/internal/store"
)

// replayRetention is how long a delivery fingerprint is remembered. It must
// comfortably exceed any sender's retry window (GitHub retries for hours) while
// keeping the table small. Rows older than this are pruned opportunistically.
const replayRetention = 72 * time.Hour

// EnsureReplaySchema creates the durable replay-protection table. Idempotent,
// and created on demand rather than in migrate/schema.sql so an existing
// TAG_HOME self-heals.
func EnsureReplaySchema(db *store.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS webhook_deliveries (
		fingerprint TEXT PRIMARY KEY,
		platform    TEXT NOT NULL,
		seen_at     TEXT NOT NULL
	)`)
	return err
}

// deliveryFingerprint derives the identity used for replay protection.
//
// An explicit delivery id (X-GitHub-Delivery, Linear-Delivery) is authoritative
// when present. When it is absent the SIGNATURE is used instead, which closes
// two real gaps:
//
//   - Generic signed webhooks carried no delivery id at all, so a captured
//     signed body could be replayed indefinitely.
//   - A captured Slack payload could be replayed freely inside the ±5m
//     timestamp tolerance. Slack signs "v0:<ts>:<body>", so the signature is a
//     stable identity for that exact (timestamp, body) pair — replaying the
//     capture necessarily replays the signature and is now caught.
//
// The signature is hashed rather than stored raw: it is derived from the shared
// secret, and a replay table is not a place to keep that material.
func deliveryFingerprint(platform, deliveryID, signature string) string {
	// The platform is DELIBERATELY not part of the key.
	//
	// It used to be, and it is caller-controlled — the path segment. Since the
	// generic/linear branch validates a bare HMAC of the body, byte-identical to
	// GitHub's, one captured github delivery replayed freely by changing the
	// path: /webhook/linear accepted it as new because the fingerprint was
	// prefixed "linear:" instead of "github:". Partitioning a replay namespace
	// by a value the attacker chooses is not a namespace.
	//
	// A delivery id and a signature are both already globally unique in
	// practice; the cost of the collision this invites — two platforms sending
	// the identical id — is one delivery rejected as a duplicate, which is the
	// safe direction.
	if deliveryID != "" {
		return "id:" + deliveryID
	}
	if signature == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(signature))
	return "sig:" + hex.EncodeToString(sum[:])
}

// markDelivered records a fingerprint durably and reports whether it was new
// (false = replay).
//
// Durability is the point. The in-memory set is bounded at 4096 entries and
// dies with the process, so replay protection silently lapsed on eviction and
// on every restart — an attacker with a captured payload only had to wait, or
// to send 4096 other events first. SQLite's PRIMARY KEY does the deduplication,
// so concurrent requests cannot both win.
//
// If the DB is unavailable this FAILS CLOSED (reports a replay) rather than
// waving the request through: an event that cannot be checked for replay is
// indistinguishable from one that is a replay, and dropping a legitimate
// webhook is recoverable where accepting a replayed job is not.
func markDelivered(db *store.DB, platform, fingerprint string) bool {
	if fingerprint == "" {
		// Nothing to key on: unsigned-and-idless. Cannot dedupe, so do not claim to.
		return true
	}
	if db == nil {
		return false
	}
	res, err := db.Exec(
		`INSERT OR IGNORE INTO webhook_deliveries(fingerprint, platform, seen_at) VALUES(?,?,?)`,
		fingerprint, platform, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false
	}
	return n > 0
}

// pruneDeliveries drops fingerprints older than replayRetention. Best-effort:
// a failure here must never reject a legitimate event.
func pruneDeliveries(db *store.DB) {
	if db == nil {
		return
	}
	cutoff := time.Now().UTC().Add(-replayRetention).Format(time.RFC3339)
	_, _ = db.Exec(`DELETE FROM webhook_deliveries WHERE seen_at < ?`, cutoff)
}
