// Tofu (2026-10-26): this file is Tofu's addition; it is not part of upstream Homebox.

package migrations

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// SnapshotHeader starts the first line of tofu/postgres_snapshot.sql; the snapshot's manifest follows it as JSON.
const SnapshotHeader = "-- tofu-schema-snapshot "

// SnapshotMigration is one migration a snapshot holds: its version, its file in the postgres folder and the SHA-256
// of that file.
type SnapshotMigration struct {
	Version int64  `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
}

// SnapshotManifest says what a snapshot holds: the migrations it was made from and the tables it makes.
type SnapshotManifest struct {
	Migrations []SnapshotMigration `json:"migrations"`
	Tables     []string            `json:"tables"`
}

//go:embed tofu/postgres_snapshot.sql
var postgresSnapshot string

// PostgresSnapshot returns the schema that the postgres migrations make on an empty database, as one SQL script, with
// its manifest. The start's schema step on Tofu (app/api/tofu_schema.go) gives an empty database this script in one
// round trip instead of running every migration's statements one at a time. app/tools/tofusnapshot makes it. ok is
// false when the embedded file holds no snapshot.
func PostgresSnapshot() (script string, manifest SnapshotManifest, ok bool) {
	first, _, found := strings.Cut(postgresSnapshot, "\n")
	if !found || !strings.HasPrefix(first, SnapshotHeader) {
		return "", SnapshotManifest{}, false
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(first, SnapshotHeader)), &manifest); err != nil {
		return "", SnapshotManifest{}, false
	}
	return postgresSnapshot, manifest, true
}
