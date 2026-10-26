package main

// Tofu (2026-10-26): this file is Tofu's addition; it is not part of upstream Homebox.
//
// The schema step of a start on PostgreSQL: goose's migrations and row-level security, while one start holds an
// advisory lock. See "Start-up when several starts run at once" in TOFU.md.
//
//   - Tofu's container hosting can start several copies of a new deployment within the same second, all on one
//     database. goose.Up has no lock of its own and runs each migration in a transaction of its own, so on an empty
//     database every copy created goose_db_version and the same tables at the same moment, and all but one stopped
//     with "relation goose_db_version already exists" or a duplicate key in pg_type or pg_class.
//   - The lock is transaction-level: pg_try_advisory_xact_lock(tofuSchemaLockKey), taken by a transaction of its own
//     that writes nothing, stays open for the whole step and is then rolled back. Tofu's DATABASE_URL is a
//     transaction pooler (port 6543): it keeps one server connection with one client from BEGIN until the
//     transaction ends and lends it to other clients only between transactions. A session-level lock
//     (pg_advisory_lock) would stay on a server connection that other clients go on using; a transaction-level lock
//     lives exactly as long as the transaction that holds it, and PostgreSQL releases it when that transaction ends,
//     however it ends. A start that dies drops its connection, and the transaction is rolled back with it.
//   - A start that finds the lock taken does not wait inside the database: it asks again with
//     pg_try_advisory_xact_lock in a short transaction, rolls back, sleeps a few hundred milliseconds and looks
//     whether the step is done, so a waiting start holds no server connection of the pooler between its questions.
//     After tofuSchemaLockWait it stops the start.
//   - Before it asks for the lock, a start looks whether the work is done: goose_db_version records every migration
//     this build embeds, and every table has row-level security. Then it skips the lock, so a restart, and the
//     starts that waited during a first deployment, finish with two queries.
//   - An empty database gets the schema snapshot (internal/data/migrations/tofu/postgres_snapshot.sql) in one round
//     trip, instead of goose sending every statement of every migration as a round trip of its own: the tables
//     goose's migrations make, goose_db_version recording them, and row-level security. goose then applies any
//     migration newer than the snapshot. A database part-way through goes through goose, as upstream's start does.
//   - Row-level security: after every goose.Up, in the same locked step, on every ordinary table in current_schema()
//     that the app's role owns, goose_db_version included. No policy is made: the database's Data API roles (anon,
//     authenticated) see no row and can write none, while the app's own connection owns the tables and is not
//     limited by it.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog/log"
	"github.com/sysadminsmedia/homebox/backend/internal/data/migrations"
)

const (
	// tofuSchemaLockKey is the advisory lock key every start of Homebox takes for its schema step: crc32 of
	// "homebox:tofu-schema". Any fixed number works, as long as every start uses the same one.
	tofuSchemaLockKey int64 = 2805111225
	// tofuSchemaLockWait bounds how long a start waits for another start's schema step.
	tofuSchemaLockWait = 150 * time.Second
)

// tofuMigratePostgres is the schema step of a start on PostgreSQL (see the top of this file). goose's base FS and
// dialect are set already; dir is the migrations' folder in it. A start that still meets an object made beside it, by
// something that did not take the lock, tries the whole step once more.
func tofuMigratePostgres(ctx context.Context, db *sql.DB, fsys fs.FS, dir string) error {
	err := tofuMigratePostgresOnce(ctx, db, fsys, dir)
	if err != nil && tofuDuplicateObject(err) {
		log.Warn().Err(err).Msg("tofu schema: the schema step met an object made beside it; trying the whole step once more")
		err = tofuMigratePostgresOnce(ctx, db, fsys, dir)
	}
	return err
}

func tofuMigratePostgresOnce(ctx context.Context, db *sql.DB, fsys fs.FS, dir string) error {
	started := time.Now()
	collected, err := goose.CollectMigrations(dir, 0, goose.MaxVersion)
	if err != nil {
		return err
	}
	versions := make([]int64, 0, len(collected))
	for _, m := range collected {
		versions = append(versions, m.Version)
	}

	done, err := tofuSchemaDone(ctx, db, versions)
	if err != nil {
		return err
	}
	if done {
		log.Info().Msg("tofu schema: up to date")
		return nil
	}

	lock, waited, err := tofuTakeSchemaLock(ctx, db, versions)
	if err != nil {
		return err
	}
	if lock == nil {
		log.Info().Str("waited", waited.Round(time.Millisecond).String()).
			Msg("tofu schema: another start did the schema step while this one waited")
		return nil
	}
	// Ends the lock's transaction, which wrote nothing, and with it the lock.
	defer func() { _ = lock.Rollback() }()

	what, err := tofuSchemaStep(ctx, db, fsys, dir, collected, versions)
	if err != nil {
		return err
	}
	log.Info().Str("took", time.Since(started).Round(time.Millisecond).String()).
		Str("waited", waited.Round(time.Millisecond).String()).Msg("tofu schema: " + what)
	return nil
}

// tofuSchemaStep is the schema work of a start that holds the lock. It runs on the app's own pool.
func tofuSchemaStep(ctx context.Context, db *sql.DB, fsys fs.FS, dir string, collected goose.Migrations, versions []int64) (string, error) {
	// Read the state again inside the lock: another start may have done the work just before this one took it.
	done, err := tofuSchemaDone(ctx, db, versions)
	if err != nil {
		return "", err
	}
	if done {
		return "up to date (another start had just done the schema step)", nil
	}

	var what []string
	snapshot, err := tofuApplySnapshot(ctx, db, fsys, dir, collected)
	if err != nil {
		return "", err
	}
	if snapshot != "" {
		what = append(what, snapshot)
	}
	if err := goose.UpContext(ctx, db, dir); err != nil {
		return "", err
	}
	what = append(what, "goose up to date")
	closed, err := tofuCloseTables(ctx, db)
	if err != nil {
		return "", err
	}
	if closed > 0 {
		what = append(what, fmt.Sprintf("turned on row-level security on %d tables", closed))
	}
	return strings.Join(what, "; "), nil
}

// tofuTakeSchemaLock returns the open transaction that holds the schema lock, or nil when another start finished the
// schema step while this one waited. Between its questions a waiting start holds no transaction, so no server
// connection of the pooler. After tofuSchemaLockWait it stops the start.
func tofuTakeSchemaLock(ctx context.Context, db *sql.DB, versions []int64) (*sql.Tx, time.Duration, error) {
	started := time.Now()
	for logged := false; ; logged = true {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("tofu schema: failed to start the lock's transaction: %w", err)
		}
		// The lock's transaction stays idle while the step runs on other connections, so the server's
		// idle_in_transaction_session_timeout must not end it. Both settings and the lock end with the transaction.
		var timeout string
		var held bool
		err = tx.QueryRowContext(ctx, "SELECT set_config('idle_in_transaction_session_timeout', '0', true), "+
			"pg_try_advisory_xact_lock($1)", tofuSchemaLockKey).Scan(&timeout, &held)
		if err != nil {
			_ = tx.Rollback()
			return nil, 0, fmt.Errorf("tofu schema: failed to ask for the schema lock: %w", err)
		}
		if held {
			return tx, time.Since(started), nil
		}
		_ = tx.Rollback()

		if !logged {
			log.Info().Msgf("tofu schema: another start holds the schema lock; waiting for it (at most %s)", tofuSchemaLockWait)
		}
		if time.Since(started) > tofuSchemaLockWait {
			log.Fatal().Msgf("tofu schema: another start has held the schema lock for over %s; stopping this start "+
				"instead of changing the schema beside it", tofuSchemaLockWait)
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(300*time.Millisecond + rand.N(400*time.Millisecond)):
		}
		done, err := tofuSchemaDone(ctx, db, versions)
		if err != nil {
			return nil, 0, err
		}
		if done {
			return nil, time.Since(started), nil
		}
	}
}

// tofuSchemaDone reports whether the schema step has nothing to do: goose_db_version records every migration this
// build embeds as applied, and every table the app's role owns in its schema has row-level security.
func tofuSchemaDone(ctx context.Context, db *sql.DB, versions []int64) (bool, error) {
	var hasVersions bool
	var open int
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL, (SELECT count(*)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND NOT c.relrowsecurity
			AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user))`).Scan(&hasVersions, &open); err != nil {
		return false, fmt.Errorf("tofu schema: failed to read the schema's state: %w", err)
	}
	if !hasVersions || open > 0 {
		return false, nil
	}
	// The newest row of each version says whether it is applied, as goose reads it.
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT ON (version_id) version_id, is_applied FROM goose_db_version
		ORDER BY version_id, id DESC`)
	if err != nil {
		return false, fmt.Errorf("tofu schema: failed to read goose_db_version: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := make(map[int64]bool)
	for rows.Next() {
		var v int64
		var isApplied bool
		if err := rows.Scan(&v, &isApplied); err != nil {
			return false, err
		}
		applied[v] = isApplied
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, v := range versions {
		if !applied[v] {
			return false, nil
		}
	}
	return true, nil
}

// tofuApplySnapshot gives an EMPTY database the schema snapshot in one round trip and says so, or returns "" when the
// database is not empty, does not make its tables in public, or the snapshot does not match this build's migrations:
// then goose makes the schema, as upstream's start does.
func tofuApplySnapshot(ctx context.Context, db *sql.DB, fsys fs.FS, dir string, collected goose.Migrations) (string, error) {
	script, manifest, ok := migrations.PostgresSnapshot()
	if !ok || len(manifest.Migrations) == 0 {
		return "", nil
	}
	held := make(map[int64]bool, len(manifest.Migrations))
	var newest int64
	for _, m := range manifest.Migrations {
		if !tofuFileMatches(fsys, path.Join(dir, m.File), m.SHA256) {
			log.Warn().Str("migration", m.File).Msg("tofu schema: the schema snapshot was made from another version of " +
				"this migration; goose makes the schema")
			return "", nil
		}
		held[m.Version] = true
		newest = max(newest, m.Version)
	}
	for _, m := range collected {
		if m.Version <= newest && !held[m.Version] {
			log.Warn().Str("migration", path.Base(m.Source)).Msg("tofu schema: the schema snapshot lacks this migration; " +
				"goose makes the schema")
			return "", nil
		}
	}

	var schema string
	var relations int
	var hasVersions bool
	if err := db.QueryRowContext(ctx, `SELECT current_schema(), (SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema()),
		to_regclass('goose_db_version') IS NOT NULL`).Scan(&schema, &relations, &hasVersions); err != nil {
		return "", fmt.Errorf("tofu schema: failed to read the schema's state: %w", err)
	}
	if schema != "public" || relations > 0 || hasVersions {
		return "", nil
	}
	// One simple query: PostgreSQL runs its statements as one transaction, so the database gets all of it or none.
	if _, err := db.ExecContext(ctx, script); err != nil {
		return "", fmt.Errorf("tofu schema: failed to apply the schema snapshot: %w", err)
	}
	return fmt.Sprintf("empty database: applied the schema snapshot of %d migrations (%d tables)",
		len(manifest.Migrations), len(manifest.Tables)), nil
}

// tofuFileMatches reports whether fsys holds the file name with this SHA-256.
func tofuFileMatches(fsys fs.FS, name, sha string) bool {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == sha
}

// tofuCloseTables turns on row-level security, in one round trip, for every ordinary table in current_schema() that
// the app's role owns and that lacks it, and returns how many it turned on. It only ever turns it on, and leaves
// tables the app's role does not own as they are.
func tofuCloseTables(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', n.nspname, c.relname)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND NOT c.relrowsecurity
			AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
		ORDER BY c.relname`)
	if err != nil {
		return 0, fmt.Errorf("tofu schema: failed to list tables without row-level security: %w", err)
	}
	var statements []string
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			_ = rows.Close()
			return 0, err
		}
		statements = append(statements, statement)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(statements) == 0 {
		return 0, nil
	}
	if _, err := db.ExecContext(ctx, strings.Join(statements, ";\n")); err != nil {
		return 0, fmt.Errorf("tofu schema: failed to turn on row-level security: %w", err)
	}
	return len(statements), nil
}

// tofuDuplicateObject reports whether err, or any error it wraps, says that something the schema step makes is there
// already: what a start meets when something that did not take the lock made it beside this one.
func tofuDuplicateObject(err error) bool {
	if err == nil {
		return false
	}
	// goose joins two errors into one when it fails to read and then to make goose_db_version, so every wrapped error
	// is looked at, not only the first one with a SQLSTATE.
	if state, ok := err.(interface{ SQLState() string }); ok {
		switch state.SQLState() {
		case "42P07", // duplicate_table: relation ... already exists
			"42710", // duplicate_object
			"42P06", // duplicate_schema
			"42723", // duplicate_function
			"42701", // duplicate_column
			"23505": // unique_violation, such as pg_type_typname_nsp_index when two CREATE TABLEs cross
			return true
		}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range wrapped.Unwrap() {
			if tofuDuplicateObject(e) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return tofuDuplicateObject(wrapped.Unwrap())
	}
	return false
}
