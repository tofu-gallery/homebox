// Tofu (2026-11-05): this package is Tofu's addition; it is not part of upstream Homebox.
//
// Package postgresblob implements a gocloud.dev/blob driver that keeps every
// object in PostgreSQL, and the wiring Homebox uses to select it.
//
// Tofu's container hosting scales an app to zero and gives a new container a
// fresh disk on every start, so Homebox's default fileblob bucket under /data
// loses item photos and attachments on every restart and deploy. A copy's
// database survives both. This driver puts the bytes in that database, in two
// tables of its own, and leaves every existing table alone:
//
//   - tofu_blob_objects has one row per key: the content the key holds, its
//     content type, size and chunk size, and when it was created and changed.
//   - tofu_blob_chunks holds each content's bytes in bytea chunks of ChunkSize
//     bytes, the last one shorter. No object is limited by a bytea value's
//     1 GiB, and neither a writer nor a long read holds more than a chunk or
//     two in memory.
//
// The URL scheme is "postgresblob". OpenBucketURL uses the process-wide pool
// registered with RegisterPool, so no second connection pool is opened:
//
//	postgresblob://
//
// A writer inserts its chunks under a new content id inside one transaction.
// At Close, in the same transaction, it points the key at that content and
// deletes the key's other chunks. Until the commit, readers see the previous
// object; a write that fails, is cancelled or is cut off by a crash rolls back
// and leaves nothing behind.
//
// A reader reads the key's row when it is opened and the bytes at its first
// Read: a range of up to EagerReadLimit bytes with one statement, served from
// memory after that, and a longer range one chunk at a time inside a read-only
// REPEATABLE READ transaction. Either way a read returns one version of the
// object whole. A delete or overwrite that commits after the first Read
// changes nothing the reader returns; one that commits between opening the
// reader and its first Read makes that Read fail with FailedPrecondition.
package postgresblob

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/gcerrors"
)

const (
	// Scheme is the URL scheme this package registers with gocloud.dev/blob.
	Scheme = "postgresblob"

	// ObjectsTable has one row per key, and ChunksTable holds the bytes. Both
	// are Tofu's own tables; no existing table is read or changed.
	ObjectsTable = "tofu_blob_objects"
	ChunksTable  = "tofu_blob_chunks"

	// ChunkSize is the size of every chunk of a new object but the last. A
	// writer holds at most one chunk in memory, and a 1 GiB restore is 256
	// inserts.
	ChunkSize = 4 << 20

	// EagerReadLimit is the longest range read with one statement and served
	// from memory. It is above Homebox's default 10 MB upload limit, so an
	// ordinary photo or attachment download holds no transaction open; a
	// backup download or a staged restore is streamed instead.
	EagerReadLimit = 16 << 20

	// streamIdleTimeout bounds how long a streamed read's snapshot may sit
	// idle between two chunks while its caller, a browser downloading a
	// backup for example, takes the last one. It replaces the server's
	// idle_in_transaction_session_timeout for that transaction only: a
	// stricter server setting does not cut a slow download off, and a
	// snapshot left open holds back vacuum for no longer than this.
	streamIdleTimeout = "15min"

	defaultContentType = "application/octet-stream"
	defaultPageSize    = 1000

	// schemaLockKey serializes table creation between starts of the same app.
	// Tofu starts several copies of a new deployment at once (see TOFU.md,
	// "Start-up when several starts run at once"); CREATE TABLE IF NOT EXISTS
	// is not safe under that race. The key differs from the schema step's
	// 2805111225 so the two never wait on each other.
	schemaLockKey = 2805111226
)

const (
	createObjectsSQL = `CREATE TABLE IF NOT EXISTS ` + ObjectsTable + ` (
	"key" text PRIMARY KEY,
	content_id uuid NOT NULL,
	content_type text NOT NULL,
	size bigint NOT NULL CHECK (size >= 0),
	chunk_size integer NOT NULL CHECK (chunk_size > 0),
	created_at timestamptz NOT NULL DEFAULT now(),
	mod_time timestamptz NOT NULL DEFAULT now()
)`

	// The chunks carry their key, so that a write can delete every chunk of
	// its key but its own without first reading which content it replaces.
	createChunksSQL = `CREATE TABLE IF NOT EXISTS ` + ChunksTable + ` (
	"key" text NOT NULL,
	content_id uuid NOT NULL,
	seq integer NOT NULL CHECK (seq >= 0),
	data bytea NOT NULL,
	PRIMARY KEY ("key", content_id, seq)
)`

	// rowSecurityOffSQL names the driver's tables that lack row-level security
	// and are owned by the app's role, the condition the schema step uses
	// (backend/app/api/tofu_schema.go, tofuCloseTables).
	rowSecurityOffSQL = `SELECT c.relname::text FROM pg_class c
WHERE c.oid IN (to_regclass($1), to_regclass($2)) AND NOT c.relrowsecurity
	AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)`

	objectSQL = `SELECT content_id::text, content_type, size, chunk_size, created_at, mod_time
FROM ` + ObjectsTable + ` WHERE "key" = $1`

	// chunkRangeSQL joins chunks $3 to $4 of one content in one statement.
	// Its snapshot holds either every chunk of that content or, once a delete
	// or overwrite of the key has committed, none of them.
	chunkRangeSQL = `SELECT string_agg(data, ''::bytea ORDER BY seq) FROM ` + ChunksTable + `
WHERE "key" = $1 AND content_id = $2::uuid AND seq BETWEEN $3::int AND $4::int`

	chunkSQL = `SELECT data FROM ` + ChunksTable + ` WHERE "key" = $1 AND content_id = $2::uuid AND seq = $3::int`

	insertChunkSQL = `INSERT INTO ` + ChunksTable + ` ("key", content_id, seq, data) VALUES ($1, $2::uuid, $3::int, $4)`

	upsertObjectSQL = `INSERT INTO ` + ObjectsTable + ` ("key", content_id, content_type, size, chunk_size, created_at, mod_time)
VALUES ($1, $2::uuid, $3, $4, $5, statement_timestamp(), statement_timestamp())
ON CONFLICT ("key") DO UPDATE SET
	content_id = EXCLUDED.content_id,
	content_type = EXCLUDED.content_type,
	size = EXCLUDED.size,
	chunk_size = EXCLUDED.chunk_size,
	mod_time = EXCLUDED.mod_time`

	insertObjectSQL = `INSERT INTO ` + ObjectsTable + ` ("key", content_id, content_type, size, chunk_size, created_at, mod_time)
VALUES ($1, $2::uuid, $3, $4, $5, statement_timestamp(), statement_timestamp())
ON CONFLICT ("key") DO NOTHING`

	// deleteOtherChunksSQL runs after the key's row points at the new
	// content, and as a statement of its own: a write to the same key that
	// held the row first has committed by then, so this statement's snapshot
	// includes that write's chunks and deletes them too.
	deleteOtherChunksSQL = `DELETE FROM ` + ChunksTable + ` WHERE "key" = $1 AND content_id <> $2::uuid`

	deleteObjectSQL = `DELETE FROM ` + ObjectsTable + ` WHERE "key" = $1`
	deleteChunksSQL = `DELETE FROM ` + ChunksTable + ` WHERE "key" = $1`

	// copySQL reads the source row and copies its chunks in one statement, so
	// both come from one snapshot, and points the destination at the copy.
	copySQL = `WITH src AS (
	SELECT content_id, content_type, size, chunk_size FROM ` + ObjectsTable + ` WHERE "key" = $2
), copied AS (
	INSERT INTO ` + ChunksTable + ` ("key", content_id, seq, data)
	SELECT $1, $3::uuid, c.seq, c.data FROM ` + ChunksTable + ` c
	JOIN src ON c."key" = $2 AND c.content_id = src.content_id
)
INSERT INTO ` + ObjectsTable + ` ("key", content_id, content_type, size, chunk_size, created_at, mod_time)
SELECT $1, $3::uuid, content_type, size, chunk_size, statement_timestamp(), statement_timestamp() FROM src
ON CONFLICT ("key") DO UPDATE SET
	content_id = EXCLUDED.content_id,
	content_type = EXCLUDED.content_type,
	size = EXCLUDED.size,
	chunk_size = EXCLUDED.chunk_size,
	mod_time = EXCLUDED.mod_time`

	// COLLATE "C" makes ORDER BY and range comparisons use UTF-8 byte order,
	// which is what the blob driver contract requires; a database's default
	// collation may order differently.
	listFlatSQL = `SELECT "key", size, mod_time FROM ` + ObjectsTable + `
WHERE starts_with("key", $1) AND ($2::text = '' OR "key" COLLATE "C" > $2 COLLATE "C")
ORDER BY "key" COLLATE "C"
LIMIT $3`

	listAllSQL = `SELECT "key", size, mod_time FROM ` + ObjectsTable + `
WHERE starts_with("key", $1)
ORDER BY "key" COLLATE "C"`
)

var (
	// errNoPool is returned when a bucket is opened through the URL scheme
	// before the application has registered its database pool.
	errNoPool = errors.New("postgresblob: no PostgreSQL pool registered; call RegisterPool at startup")

	// errWriterClosed is returned when Write or Close is called after Close.
	errWriterClosed = errors.New("postgresblob: writer is already closed")

	// errUnsupportedOptions is returned for a write that asks for something
	// the driver does not store, rather than silently dropping it. Homebox
	// sets only a content type and an MD5, which the SDK itself checks.
	errUnsupportedOptions = fmt.Errorf(
		"postgresblob: metadata, Cache-Control, Content-Disposition, Content-Encoding and Content-Language are not stored: %w",
		gcerrors.ErrUnimplemented)

	schemeOnce sync.Once

	poolMu sync.RWMutex
	poolDB *sql.DB
)

// RegisterScheme registers the postgresblob URL scheme with
// gocloud.dev/blob. It is safe to call more than once. The application gets
// this through RegisterPool; tests may call it directly.
func RegisterScheme() {
	schemeOnce.Do(func() {
		blob.DefaultURLMux().RegisterBucket(Scheme, &URLOpener{})
	})
}

// RegisterPool registers db as the connection pool behind postgresblob:// and
// creates the tables if they do not exist yet. The pool stays owned by the
// caller: the driver never closes it. db must be a PostgreSQL pool.
//
// It is called once at startup, before any bucket is opened.
func RegisterPool(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errNoPool
	}
	RegisterScheme()
	if err := ensureSchema(ctx, db); err != nil {
		return err
	}
	poolMu.Lock()
	poolDB = db
	poolMu.Unlock()
	return nil
}

func currentPool() (*sql.DB, error) {
	poolMu.RLock()
	defer poolMu.RUnlock()
	if poolDB == nil {
		return nil, errNoPool
	}
	return poolDB, nil
}

// URLOpener opens postgresblob:// URLs on the pool registered by
// RegisterPool. No other URL form is accepted.
type URLOpener struct{}

// OpenBucketURL implements blob.BucketURLOpener.
func (*URLOpener) OpenBucketURL(ctx context.Context, u *url.URL) (*blob.Bucket, error) {
	if u.Host != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("postgresblob: unsupported URL %q; use %s://", u.String(), Scheme)
	}
	if len(u.Query()) > 0 {
		return nil, fmt.Errorf("postgresblob: unsupported query parameters in %q", u.String())
	}
	db, err := currentPool()
	if err != nil {
		return nil, err
	}
	return blob.NewBucket(newDriverBucket(db, true)), nil
}

// NewBucket returns a bucket on db that creates the tables on its first
// operation. The application uses the URL scheme instead, so that the pool is
// registered in one place; this constructor exists for tests and for callers
// that already hold a pool.
func NewBucket(db *sql.DB) *blob.Bucket {
	return blob.NewBucket(newDriverBucket(db, false))
}

func newDriverBucket(db *sql.DB, schemaReady bool) *bucket {
	return &bucket{db: db, chunkSize: ChunkSize, eagerLimit: EagerReadLimit, schemaReady: schemaReady}
}

// ensureSchema creates the tables once per pool and turns row-level security
// on. The transaction-level advisory lock makes concurrent starts take turns;
// without it, two CREATE TABLE IF NOT EXISTS statements can still collide in
// PostgreSQL's catalog.
func ensureSchema(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresblob: begin schema transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("postgresblob: take schema lock: %w", err)
	}
	for _, statement := range []string{createObjectsSQL, createChunksSQL} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("postgresblob: create tables: %w", err)
		}
	}

	// The app's role owns the tables and is not limited by row security, while
	// the database's Data API roles see no row. The schema step does this for
	// every table that exists when a start begins; these tables are created
	// after it, so it is done here. Like the schema step, it only turns row
	// security on where it is off and the app's role owns the table: ALTER
	// TABLE takes an ACCESS EXCLUSIVE lock, and on every later start that lock
	// would make the instances still serving wait to read or write attachments.
	rows, err := tx.QueryContext(ctx, rowSecurityOffSQL, ObjectsTable, ChunksTable)
	if err != nil {
		return fmt.Errorf("postgresblob: read row-level security: %w", err)
	}
	var open []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("postgresblob: read row-level security: %w", err)
		}
		open = append(open, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgresblob: read row-level security: %w", err)
	}
	_ = rows.Close()
	for _, name := range open {
		var table string
		switch name {
		case ObjectsTable:
			table = ObjectsTable
		case ChunksTable:
			table = ChunksTable
		default:
			continue
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` ENABLE ROW LEVEL SECURITY`); err != nil {
			return fmt.Errorf("postgresblob: enable row-level security on %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresblob: commit schema: %w", err)
	}
	return nil
}

type bucket struct {
	db         *sql.DB
	chunkSize  int   // bytes per chunk of a new object; ChunkSize outside tests
	eagerLimit int64 // longest range read in one statement; EagerReadLimit outside tests

	schemaMu    sync.Mutex
	schemaReady bool
}

func (b *bucket) ensure(ctx context.Context) error {
	b.schemaMu.Lock()
	defer b.schemaMu.Unlock()
	if b.schemaReady {
		return nil
	}
	if err := ensureSchema(ctx, b.db); err != nil {
		return err
	}
	b.schemaReady = true
	return nil
}

// ErrorCode implements driver.Bucket.
func (*bucket) ErrorCode(err error) gcerrors.ErrorCode {
	switch {
	case errors.Is(err, gcerrors.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		return gcerrors.NotFound
	case errors.Is(err, gcerrors.ErrFailedPrecondition):
		return gcerrors.FailedPrecondition
	case errors.Is(err, gcerrors.ErrInvalidArgument):
		return gcerrors.InvalidArgument
	case errors.Is(err, gcerrors.ErrUnimplemented):
		return gcerrors.Unimplemented
	case errors.Is(err, gcerrors.ErrInternal):
		return gcerrors.Internal
	case errors.Is(err, context.Canceled):
		return gcerrors.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return gcerrors.DeadlineExceeded
	default:
		return gcerrors.Unknown
	}
}

// As implements driver.Bucket. The driver exposes no types of its own.
func (*bucket) As(any) bool { return false }

// ErrorAs implements driver.Bucket. The driver exposes no types of its own.
func (*bucket) ErrorAs(error, any) bool { return false }

// Close implements driver.Bucket. The database pool belongs to the
// application and is left open.
func (*bucket) Close() error { return nil }

// Attributes implements driver.Bucket.
func (b *bucket) Attributes(ctx context.Context, key string) (*driver.Attributes, error) {
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	obj, err := b.object(ctx, key)
	if err != nil {
		return nil, err
	}
	return &driver.Attributes{
		ContentType: normalizeContentType(obj.contentType),
		Size:        obj.size,
		CreateTime:  obj.created,
		ModTime:     obj.modified,
	}, nil
}

type object struct {
	contentID   string
	contentType string
	size        int64
	chunkSize   int64
	created     time.Time
	modified    time.Time
}

func (b *bucket) object(ctx context.Context, key string) (object, error) {
	var obj object
	err := b.db.QueryRowContext(ctx, objectSQL, key).
		Scan(&obj.contentID, &obj.contentType, &obj.size, &obj.chunkSize, &obj.created, &obj.modified)
	if errors.Is(err, sql.ErrNoRows) {
		return object{}, notFound(key)
	}
	if err != nil {
		return object{}, fmt.Errorf("postgresblob: read %q: %w", key, err)
	}
	if obj.chunkSize <= 0 {
		return object{}, fmt.Errorf("postgresblob: %q has chunk size %d: %w", key, obj.chunkSize, gcerrors.ErrInternal)
	}
	return obj, nil
}

// NewRangeReader implements driver.Bucket. It reads the key's row now and
// the bytes at the first Read, so a conditional request that the server
// answers with 304 reads no content.
func (b *bucket) NewRangeReader(ctx context.Context, key string, offset, length int64, opts *driver.ReaderOptions) (driver.Reader, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%w: negative offset %d", gcerrors.ErrInvalidArgument, offset)
	}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	if err := callBefore(opts.BeforeRead); err != nil {
		return nil, err
	}
	obj, err := b.object(ctx, key)
	if err != nil {
		return nil, err
	}
	start := min(offset, obj.size)
	end := obj.size
	if length >= 0 && length < obj.size-start {
		end = start + length
	}
	return &reader{
		db:        b.db,
		ctx:       ctx,
		key:       key,
		contentID: obj.contentID,
		chunkSize: obj.chunkSize,
		pos:       start,
		end:       end,
		stream:    end-start > b.eagerLimit,
		attrs: driver.ReaderAttributes{
			ContentType: normalizeContentType(obj.contentType),
			ModTime:     obj.modified,
			Size:        obj.size,
		},
	}, nil
}

type reader struct {
	db        *sql.DB
	ctx       context.Context
	key       string
	contentID string
	chunkSize int64
	pos, end  int64 // the next byte to return, and the end of the range (exclusive)
	stream    bool  // read chunk by chunk in a snapshot, not all at once

	started bool
	buf     []byte  // bytes fetched and not yet returned
	tx      *sql.Tx // the snapshot of a streamed read
	err     error
	attrs   driver.ReaderAttributes
}

// Read implements io.Reader.
func (r *reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for len(r.buf) == 0 {
		if r.pos >= r.end {
			r.finish()
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			r.err = err
			r.finish()
			return 0, err
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	r.pos += int64(n)
	return n, nil
}

// fill fetches the next bytes to return: the whole range on a short read, the
// next chunk on a streamed one.
func (r *reader) fill() error {
	seq := r.pos / r.chunkSize
	if !r.stream {
		if r.started {
			return r.missing(seq)
		}
		r.started = true
		last := (r.end - 1) / r.chunkSize
		var data []byte
		if err := r.db.QueryRowContext(r.ctx, chunkRangeSQL, r.key, r.contentID, seq, last).Scan(&data); err != nil {
			return fmt.Errorf("postgresblob: read %q: %w", r.key, err)
		}
		return r.keep(data, seq, r.end, true)
	}

	first := !r.started
	if first {
		tx, err := r.db.BeginTx(r.ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return fmt.Errorf("postgresblob: read %q: begin: %w", r.key, err)
		}
		if _, err := tx.ExecContext(r.ctx, `SELECT set_config('idle_in_transaction_session_timeout', $1, true)`,
			streamIdleTimeout); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("postgresblob: read %q: %w", r.key, err)
		}
		r.tx = tx
		r.started = true
	}
	var data []byte
	err := r.tx.QueryRowContext(r.ctx, chunkSQL, r.key, r.contentID, seq).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		data = nil
	} else if err != nil {
		return fmt.Errorf("postgresblob: read %q: chunk %d: %w", r.key, seq, err)
	}
	return r.keep(data, seq, min(r.end, (seq+1)*r.chunkSize), first)
}

// keep stores the part of data, which begins at chunk seq, that the reader
// returns next, up to the absolute offset until.
func (r *reader) keep(data []byte, seq, until int64, first bool) error {
	if len(data) == 0 {
		if first {
			return fmt.Errorf("postgresblob: %q was replaced or deleted after the reader was opened; open it again: %w",
				r.key, gcerrors.ErrFailedPrecondition)
		}
		return r.missing(seq)
	}
	from, to := r.pos-seq*r.chunkSize, until-seq*r.chunkSize
	if from < 0 || to <= from || int64(len(data)) < to {
		return r.missing(seq)
	}
	r.buf = data[from:to]
	return nil
}

func (r *reader) missing(seq int64) error {
	return fmt.Errorf("postgresblob: %q is missing part of its content at chunk %d: %w", r.key, seq, gcerrors.ErrInternal)
}

// finish ends the snapshot of a streamed read.
func (r *reader) finish() {
	if r.tx != nil {
		_ = r.tx.Commit()
		r.tx = nil
	}
}

// Close implements driver.Reader.
func (r *reader) Close() error {
	r.finish()
	r.buf = nil
	return nil
}

// Attributes implements driver.Reader.
func (r *reader) Attributes() *driver.ReaderAttributes { return &r.attrs }

// As implements driver.Reader.
func (*reader) As(any) bool { return false }

// NewTypedWriter implements driver.Bucket.
func (b *bucket) NewTypedWriter(ctx context.Context, key, contentType string, opts *driver.WriterOptions) (driver.Writer, error) {
	if len(opts.Metadata) > 0 || opts.CacheControl != "" || opts.ContentDisposition != "" ||
		opts.ContentEncoding != "" || opts.ContentLanguage != "" {
		return nil, errUnsupportedOptions
	}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	if err := callBefore(opts.BeforeWrite); err != nil {
		return nil, err
	}
	return &writer{
		db:          b.db,
		ctx:         ctx,
		key:         key,
		contentType: normalizeContentType(contentType),
		ifNotExist:  opts.IfNotExist,
		contentID:   uuid.NewString(),
		chunkSize:   b.chunkSize,
	}, nil
}

type writer struct {
	db          *sql.DB
	ctx         context.Context
	key         string
	contentType string
	ifNotExist  bool
	contentID   string
	chunkSize   int

	tx     *sql.Tx // begun with the first chunk; holds every chunk until Close commits
	buf    []byte  // the next chunk, not yet full
	seq    int32
	size   int64
	err    error
	closed bool
}

// Write implements io.Writer. It sends each full chunk to the database as it
// fills, so the writer holds at most one chunk.
func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errWriterClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		if len(w.buf) == 0 && len(p) >= w.chunkSize {
			// A whole chunk straight from the caller's slice; pgx has encoded
			// it by the time the insert returns, so the slice is not kept.
			if err := w.insertChunk(p[:w.chunkSize]); err != nil {
				return written, w.fail(err)
			}
			p = p[w.chunkSize:]
			written += w.chunkSize
			continue
		}
		take := min(w.chunkSize-len(w.buf), len(p))
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		written += take
		if len(w.buf) == w.chunkSize {
			if err := w.insertChunk(w.buf); err != nil {
				return written, w.fail(err)
			}
			w.buf = w.buf[:0]
		}
	}
	return written, nil
}

func (w *writer) insertChunk(data []byte) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.seq == math.MaxInt32 {
		return fmt.Errorf("postgresblob: %q has more than %d chunks: %w", w.key, math.MaxInt32, gcerrors.ErrInvalidArgument)
	}
	if err := w.begin(); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(w.ctx, insertChunkSQL, w.key, w.contentID, w.seq, data); err != nil {
		return fmt.Errorf("postgresblob: write %q: chunk %d: %w", w.key, w.seq, err)
	}
	w.seq++
	w.size += int64(len(data))
	return nil
}

func (w *writer) begin() error {
	if w.tx != nil {
		return nil
	}
	tx, err := w.db.BeginTx(w.ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresblob: write %q: begin: %w", w.key, err)
	}
	w.tx = tx
	return nil
}

// fail records err and rolls back, so nothing the writer sent stays behind.
func (w *writer) fail(err error) error {
	w.err = err
	w.rollback()
	return err
}

func (w *writer) rollback() {
	if w.tx != nil {
		_ = w.tx.Rollback()
		w.tx = nil
	}
	w.buf = nil
}

// Close implements io.Closer. It sends the last chunk, points the key at the
// new content, deletes the key's other chunks and commits, all in the
// writer's one transaction.
func (w *writer) Close() error {
	if w.closed {
		return errWriterClosed
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.rollback()
		return err
	}
	if len(w.buf) > 0 {
		if err := w.insertChunk(w.buf); err != nil {
			w.rollback()
			return err
		}
	}
	w.buf = nil
	if err := w.begin(); err != nil {
		w.rollback()
		return err
	}
	if err := w.publish(); err != nil {
		w.rollback()
		return err
	}
	err := w.tx.Commit()
	w.tx = nil
	if err != nil {
		return fmt.Errorf("postgresblob: write %q: commit: %w", w.key, err)
	}
	return nil
}

func (w *writer) publish() error {
	if w.ifNotExist {
		res, err := w.tx.ExecContext(w.ctx, insertObjectSQL, w.key, w.contentID, w.contentType, w.size, w.chunkSize)
		if err != nil {
			return fmt.Errorf("postgresblob: write %q: %w", w.key, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("postgresblob: write %q: rows affected: %w", w.key, err)
		}
		if affected == 0 {
			return fmt.Errorf("postgresblob: object %q already exists: %w", w.key, gcerrors.ErrFailedPrecondition)
		}
		return nil
	}
	if _, err := w.tx.ExecContext(w.ctx, upsertObjectSQL, w.key, w.contentID, w.contentType, w.size, w.chunkSize); err != nil {
		return fmt.Errorf("postgresblob: write %q: %w", w.key, err)
	}
	if _, err := w.tx.ExecContext(w.ctx, deleteOtherChunksSQL, w.key, w.contentID); err != nil {
		return fmt.Errorf("postgresblob: write %q: delete the content it replaced: %w", w.key, err)
	}
	return nil
}

// Delete implements driver.Bucket. The row goes first: if a write to the key
// holds the row, the delete waits for it, and the chunk delete that follows
// sees that write's chunks.
func (b *bucket) Delete(ctx context.Context, key string, opts *driver.DeleteOptions) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	if err := callBefore(opts.BeforeDelete); err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresblob: delete %q: begin: %w", key, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, deleteObjectSQL, key)
	if err != nil {
		return fmt.Errorf("postgresblob: delete %q: %w", key, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgresblob: delete %q: rows affected: %w", key, err)
	}
	if affected == 0 {
		return notFound(key)
	}
	if _, err := tx.ExecContext(ctx, deleteChunksSQL, key); err != nil {
		return fmt.Errorf("postgresblob: delete %q: chunks: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresblob: delete %q: commit: %w", key, err)
	}
	return nil
}

// Copy implements driver.Bucket. It copies inside the database, without
// moving the bytes through the client.
func (b *bucket) Copy(ctx context.Context, dstKey, srcKey string, opts *driver.CopyOptions) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	if err := callBefore(opts.BeforeCopy); err != nil {
		return err
	}
	contentID := uuid.NewString()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresblob: copy %q to %q: begin: %w", srcKey, dstKey, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, copySQL, dstKey, srcKey, contentID)
	if err != nil {
		return fmt.Errorf("postgresblob: copy %q to %q: %w", srcKey, dstKey, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgresblob: copy %q to %q: rows affected: %w", srcKey, dstKey, err)
	}
	if affected == 0 {
		return notFound(srcKey)
	}
	if _, err := tx.ExecContext(ctx, deleteOtherChunksSQL, dstKey, contentID); err != nil {
		return fmt.Errorf("postgresblob: copy %q to %q: delete the content it replaced: %w", srcKey, dstKey, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresblob: copy %q to %q: commit: %w", srcKey, dstKey, err)
	}
	return nil
}

// ListPaged implements driver.Bucket. Without a delimiter it pages by key in
// byte order; with one it collapses keys below a directory into one entry and
// pages the resulting entries. Homebox itself does not list the bucket; this
// exists because the driver interface requires it.
func (b *bucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	if err := callBefore(opts.BeforeList); err != nil {
		return nil, err
	}
	if opts.Delimiter != "" {
		return b.listDelimited(ctx, opts)
	}
	return b.listFlat(ctx, opts)
}

func (b *bucket) listFlat(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	rows, err := b.db.QueryContext(ctx, listFlatSQL, opts.Prefix, string(opts.PageToken), pageSize+1)
	if err != nil {
		return nil, fmt.Errorf("postgresblob: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	objects := make([]*driver.ListObject, 0, pageSize)
	for rows.Next() {
		var (
			key      string
			size     int64
			modified time.Time
		)
		if err := rows.Scan(&key, &size, &modified); err != nil {
			return nil, fmt.Errorf("postgresblob: list: scan: %w", err)
		}
		objects = append(objects, &driver.ListObject{Key: key, Size: size, ModTime: modified})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresblob: list: %w", err)
	}

	var next []byte
	if len(objects) > pageSize {
		objects = objects[:pageSize]
		next = []byte(objects[pageSize-1].Key)
	}
	return &driver.ListPage{Objects: objects, NextPageToken: next}, nil
}

func (b *bucket) listDelimited(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	rows, err := b.db.QueryContext(ctx, listAllSQL, opts.Prefix)
	if err != nil {
		return nil, fmt.Errorf("postgresblob: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]*driver.ListObject, 0)
	lastDir := ""
	for rows.Next() {
		var (
			key      string
			size     int64
			modified time.Time
		)
		if err := rows.Scan(&key, &size, &modified); err != nil {
			return nil, fmt.Errorf("postgresblob: list: scan: %w", err)
		}
		rest := strings.TrimPrefix(key, opts.Prefix)
		if i := strings.Index(rest, opts.Delimiter); i >= 0 {
			dir := opts.Prefix + rest[:i+len(opts.Delimiter)]
			if dir != lastDir {
				entries = append(entries, &driver.ListObject{Key: dir, IsDir: true})
				lastDir = dir
			}
			continue
		}
		entries = append(entries, &driver.ListObject{Key: key, Size: size, ModTime: modified})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresblob: list: %w", err)
	}

	offset := 0
	if len(opts.PageToken) > 0 {
		offset, err = strconv.Atoi(string(opts.PageToken))
		if err != nil || offset < 0 {
			return nil, fmt.Errorf("%w: bad page token %q", gcerrors.ErrInvalidArgument, opts.PageToken)
		}
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	pageSize := opts.PageSize
	if pageSize <= 0 || offset+pageSize > len(entries) {
		pageSize = len(entries) - offset
	}
	end := offset + pageSize
	var next []byte
	if end < len(entries) {
		next = []byte(strconv.Itoa(end))
	}
	return &driver.ListPage{Objects: entries[offset:end], NextPageToken: next}, nil
}

// SignedURL implements driver.Bucket. PostgreSQL has no URL a client can use
// directly, so this reports the documented Unimplemented code rather than
// pretending.
func (*bucket) SignedURL(context.Context, string, *driver.SignedURLOptions) (string, error) {
	return "", fmt.Errorf("postgresblob: signed URLs are not supported: %w", gcerrors.ErrUnimplemented)
}

func callBefore(fn func(asFunc func(any) bool) error) error {
	if fn == nil {
		return nil
	}
	return fn(func(any) bool { return false })
}

func normalizeContentType(contentType string) string {
	if contentType == "" {
		return defaultContentType
	}
	return contentType
}

func notFound(key string) error {
	return fmt.Errorf("postgresblob: object %q: %w", key, gcerrors.ErrNotFound)
}

// Compile-time checks that the driver types satisfy the gocloud interfaces.
var (
	_ io.WriteCloser = (*writer)(nil)
	_ driver.Reader  = (*reader)(nil)
	_ driver.Bucket  = (*bucket)(nil)
)
