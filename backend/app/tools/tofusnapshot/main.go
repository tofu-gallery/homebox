// Tofu (2026-10-26): this file is Tofu's addition; it is not part of upstream Homebox.

// tofusnapshot writes internal/data/migrations/tofu/postgres_snapshot.sql: the schema that Homebox's postgres
// migrations make on an empty database, which the start's schema step on Tofu (app/api/tofu_schema.go) gives an empty
// database in one round trip. Run it from backend/ against an EMPTY scratch PostgreSQL database, with pg_dump on PATH:
//
//	go run ./app/tools/tofusnapshot 'host=127.0.0.1 port=5432 dbname=scratch user=me sslmode=disable'
//
// It applies the migrations with goose exactly as upstream's start does, then reads the schema back with pg_dump. Run
// it again when the postgres migrations change. Until then a start uses the snapshot only while every migration file
// it was made from is unchanged, and goose applies the migrations added after it.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/sysadminsmedia/homebox/backend/internal/data/migrations"
	_ "github.com/sysadminsmedia/homebox/backend/internal/data/migrations/postgres"
)

const out = "internal/data/migrations/tofu/postgres_snapshot.sql"

var createTable = regexp.MustCompile(`(?m)^CREATE TABLE public\.("?)(\w+)("?) \(`)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./app/tools/tofusnapshot '<connection string of an EMPTY PostgreSQL database>'")
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "tofusnapshot:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	var schema string
	var relations int
	if err := db.QueryRowContext(ctx, `SELECT current_schema(), (SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema())`).Scan(&schema, &relations); err != nil {
		return err
	}
	if schema != "public" || relations != 0 {
		return fmt.Errorf("the database must be empty and make its tables in public (schema %q holds %d relations)", schema, relations)
	}

	fsys, err := migrations.Migrations("postgres")
	if err != nil {
		return err
	}
	goose.SetBaseFS(fsys)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, db, "postgres"); err != nil {
		return err
	}
	collected, err := goose.CollectMigrations("postgres", 0, goose.MaxVersion)
	if err != nil {
		return err
	}
	manifest := migrations.SnapshotManifest{}
	for _, m := range collected {
		file := path.Base(m.Source)
		data, err := fs.ReadFile(fsys, path.Join("postgres", file))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		manifest.Migrations = append(manifest.Migrations, migrations.SnapshotMigration{
			Version: m.Version, File: file, SHA256: hex.EncodeToString(sum[:]),
		})
	}

	rows, err := db.QueryContext(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id`)
	if err != nil {
		return err
	}
	var versions []string
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		versions = append(versions, fmt.Sprintf("(%d, true)", v))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(versions) != len(collected)+1 {
		return fmt.Errorf("goose_db_version holds %d versions, expected %d", len(versions), len(collected)+1)
	}

	dump, err := exec.CommandContext(ctx, "pg_dump", "--dbname="+dsn, "--schema=public", "--no-owner",
		"--no-privileges", "--no-comments", "--no-publications", "--no-subscriptions", "--no-security-labels",
		"--no-tablespaces", "--inserts", "--rows-per-insert=1000", "--exclude-table-data=public.goose_db_version").Output()
	if err != nil {
		return fmt.Errorf("pg_dump: %w", err)
	}

	// Keep the statements that make the schema and its rows. Drop pg_dump's session settings, which would stay on the
	// pooler's server connection after the script, its psql commands, the public schema (which exists) and the
	// position of goose_db_version's identity (the script's own insert below moves it).
	var kept []string
	for _, line := range strings.Split(string(dump), "\n") {
		switch {
		case line == "", strings.HasPrefix(line, "--"), strings.HasPrefix(line, `\`), strings.HasPrefix(line, "SET "),
			strings.HasPrefix(line, "SELECT pg_catalog.set_config("), line == "CREATE SCHEMA public;",
			strings.HasPrefix(line, "SELECT pg_catalog.setval('public.goose_db_version_id_seq'"):
			continue
		}
		kept = append(kept, line)
	}
	body := strings.Join(kept, "\n")
	for _, word := range []string{"COPY ", "OWNER TO", "GRANT ", "REVOKE ", "CREATE EXTENSION", "CREATE SCHEMA", "set_config"} {
		if strings.Contains(body, word) {
			return fmt.Errorf("unexpected %q in pg_dump's output", word)
		}
	}
	var rls []string
	for _, m := range createTable.FindAllStringSubmatch(body, -1) {
		manifest.Tables = append(manifest.Tables, m[2])
		rls = append(rls, fmt.Sprintf("ALTER TABLE public.%q ENABLE ROW LEVEL SECURITY;", m[2]))
	}

	header, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(migrations.SnapshotHeader + string(header) + "\n")
	b.WriteString("-- Tofu (2026-10-26): made by app/tools/tofusnapshot from a database that goose's postgres migrations " +
		"brought up to date. Do not edit; run the tool again.\n")
	b.WriteString(body + "\n")
	b.WriteString("INSERT INTO public.goose_db_version (version_id, is_applied) VALUES\n\t" +
		strings.Join(versions, ",\n\t") + ";\n")
	b.WriteString(strings.Join(rls, "\n") + "\n")
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("tofusnapshot: wrote %s: %d migrations, %d tables\n", out, len(manifest.Migrations), len(manifest.Tables))
	return nil
}
