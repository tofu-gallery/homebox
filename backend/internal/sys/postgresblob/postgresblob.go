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
//   - tofu_blob_objects has one row per key: the content the key holds, the
//     attributes the SDK keeps with an object (content type, Cache-Control,
//     Content-Disposition, Content-Encoding, Content-Language, metadata and
//     the MD5 of the bytes), its size and chunk size, and when it was created
//     and changed.
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
// A reader reads the key's row when it is opened, and the bytes from its first
// Read on, in pieces that never cross a chunk: the first piece is what that
// Read asks for and at least 64 KiB, and each later one at least twice the one
// before, up to a whole chunk. A range of up to ShortReadLimit bytes reads
// each piece with a statement of its own; a longer one reads them all inside
// one read-only REPEATABLE READ transaction. No read returns bytes of two
// versions of an object. A delete or overwrite that commits while a short
// range is read makes its next piece fail with FailedPrecondition. A long
// range fails that way only when the change commits before its first Read;
// one that commits later changes nothing it returns.
//
// A key may be any UTF-8 string except one that contains U+0000, which a
// PostgreSQL text value cannot hold; every operation refuses such a key with
// InvalidArgument. Keys sort and compare in UTF-8 byte order whatever the
// database's default collation. SignedURL returns Unimplemented: PostgreSQL
// has no address a client could fetch an object from.
package postgresblob

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

	// ShortReadLimit is the longest range a reader reads without a
	// transaction, each piece with a statement of its own, so that a download
	// holds no connection while the client takes its bytes. It is above
	// Homebox's default 10 MB upload limit, so every photo or attachment up to
	// that limit is a short read; a backup download or a staged restore is
	// read inside one snapshot instead.
	ShortReadLimit = 16 << 20

	// firstFetch is the least a reader's first piece asks for; each later
	// piece asks for twice as much as the one before, up to a chunk. After a
	// Seek the blob SDK reopens its reader from the new offset to the end of
	// the object, which is what an HTTP byte range becomes, so the reader
	// never learns how much its caller wants. This keeps a range of a few
	// bytes to one piece of 64 KiB, and a whole object to a few statements
	// more than it has chunks.
	firstFetch = 64 << 10

	// streamIdleTimeout bounds how long a long range's snapshot may sit idle
	// between two pieces while its caller, a browser downloading a backup for
	// example, takes the last one. It replaces the server's
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
	// Both key columns use the C collation, so keys sort and compare in UTF-8
	// byte order, as the blob driver contract asks, whatever the database's
	// default collation; under C, the primary key's index also serves a
	// listing's starts_with as a range and returns it in order.
	//
	// metadata holds the object's metadata as a JSON object, or '' when it has
	// none. It is text rather than jsonb because jsonb cannot hold U+0000,
	// which a metadata key or value may contain.
	createObjectsSQL = `CREATE TABLE IF NOT EXISTS ` + ObjectsTable + ` (
	"key" text COLLATE "C" PRIMARY KEY,
	content_id uuid NOT NULL,
	content_type text NOT NULL,
	cache_control text NOT NULL DEFAULT '',
	content_disposition text NOT NULL DEFAULT '',
	content_encoding text NOT NULL DEFAULT '',
	content_language text NOT NULL DEFAULT '',
	metadata text NOT NULL DEFAULT '',
	md5 bytea NOT NULL,
	size bigint NOT NULL CHECK (size >= 0),
	chunk_size integer NOT NULL CHECK (chunk_size > 0),
	created_at timestamptz NOT NULL DEFAULT now(),
	mod_time timestamptz NOT NULL DEFAULT now()
)`

	// The chunks carry their key, so that a write can delete every chunk of
	// its key but its own without first reading which content it replaces.
	createChunksSQL = `CREATE TABLE IF NOT EXISTS ` + ChunksTable + ` (
	"key" text COLLATE "C" NOT NULL,
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

	objectSQL = `SELECT content_id::text, content_type, cache_control, content_disposition, content_encoding,
	content_language, metadata, md5, size, chunk_size, created_at, mod_time
FROM ` + ObjectsTable + ` WHERE "key" = $1`

	// pieceSQL reads $5 bytes of chunk $3 of one content, from byte $4 of the
	// chunk on (counted from 1). A chunk is never changed once written, so
	// every piece of a content is from the same bytes, or the chunk is gone
	// with its content. Where PostgreSQL keeps a chunk uncompressed, as it
	// keeps the bytes of a photo or of any other file that is compressed
	// already, substring reads only the part of the stored value it returns.
	pieceSQL = `SELECT substring(data FROM $4::int FOR $5::int) FROM ` + ChunksTable + `
WHERE "key" = $1 AND content_id = $2::uuid AND seq = $3::int`

	contentIDSQL = `SELECT content_id::text FROM ` + ObjectsTable + ` WHERE "key" = $1`

	insertChunkSQL = `INSERT INTO ` + ChunksTable + ` ("key", content_id, seq, data) VALUES ($1, $2::uuid, $3::int, $4)`

	// A write replaces the object: every attribute, its creation time
	// included, is the new object's.
	replaceObject = ` ON CONFLICT ("key") DO UPDATE SET
	content_id = EXCLUDED.content_id,
	content_type = EXCLUDED.content_type,
	cache_control = EXCLUDED.cache_control,
	content_disposition = EXCLUDED.content_disposition,
	content_encoding = EXCLUDED.content_encoding,
	content_language = EXCLUDED.content_language,
	metadata = EXCLUDED.metadata,
	md5 = EXCLUDED.md5,
	size = EXCLUDED.size,
	chunk_size = EXCLUDED.chunk_size,
	created_at = EXCLUDED.created_at,
	mod_time = EXCLUDED.mod_time`

	objectColumns = `("key", content_id, content_type, cache_control, content_disposition, content_encoding,
	content_language, metadata, md5, size, chunk_size, created_at, mod_time)`

	insertObjectValues = `INSERT INTO ` + ObjectsTable + ` ` + objectColumns + `
VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, statement_timestamp(), statement_timestamp())`

	upsertObjectSQL = insertObjectValues + replaceObject

	insertObjectSQL = insertObjectValues + ` ON CONFLICT ("key") DO NOTHING`

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
	SELECT content_id, content_type, cache_control, content_disposition, content_encoding, content_language,
		metadata, md5, size, chunk_size
	FROM ` + ObjectsTable + ` WHERE "key" = $2
), copied AS (
	INSERT INTO ` + ChunksTable + ` ("key", content_id, seq, data)
	SELECT $1, $3::uuid, c.seq, c.data FROM ` + ChunksTable + ` c
	JOIN src ON c."key" = $2 AND c.content_id = src.content_id
)
INSERT INTO ` + ObjectsTable + ` ` + objectColumns + `
SELECT $1, $3::uuid, content_type, cache_control, content_disposition, content_encoding, content_language,
	metadata, md5, size, chunk_size, statement_timestamp(), statement_timestamp()
FROM src` + replaceObject

	// The key columns are C already; the COLLATE "C" here keeps ORDER BY and
	// the comparison with the page token in byte order on a table whose key
	// column has another collation too. $2 is the last key of the page before,
	// or '' for the first page; no key is empty.
	listFlatSQL = `SELECT "key", size, mod_time, md5 FROM ` + ObjectsTable + `
WHERE starts_with("key", $1) AND "key" COLLATE "C" > $2 COLLATE "C"
ORDER BY "key" COLLATE "C"
LIMIT $3`

	listAfterSQL = `SELECT "key", size, mod_time, md5 FROM ` + ObjectsTable + `
WHERE starts_with("key", $1) AND "key" COLLATE "C" > $2 COLLATE "C"
ORDER BY "key" COLLATE "C"`
)

var (
	// errNoPool is returned when a bucket is opened through the URL scheme
	// before the application has registered its database pool.
	errNoPool = errors.New("postgresblob: no PostgreSQL pool registered; call RegisterPool at startup")

	// errWriterClosed is returned when Write or Close is called after Close.
	errWriterClosed = errors.New("postgresblob: writer is already closed")

	// errReaderClosed is returned by a Read after Close.
	errReaderClosed = fmt.Errorf("postgresblob: reader is closed: %w", gcerrors.ErrFailedPrecondition)

	// readCommitted is the isolation of every transaction that writes. The
	// writer, Delete and Copy rely on each statement seeing what committed
	// before it began (see deleteOtherChunksSQL), so they ask for it rather
	// than take the database's default, which may be REPEATABLE READ.
	readCommitted = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

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
	return &bucket{db: db, chunkSize: ChunkSize, shortLimit: ShortReadLimit, firstFetch: firstFetch,
		schemaReady: schemaReady}
}

// ensureSchema creates the tables once per pool and turns row-level security
// on. The transaction-level advisory lock makes concurrent starts take turns;
// without it, two CREATE TABLE IF NOT EXISTS statements can still collide in
// PostgreSQL's catalog.
func ensureSchema(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// READ COMMITTED, so that the row-level security read below sees a table
	// that another start created and committed while this one waited for the
	// lock.
	tx, err := db.BeginTx(ctx, readCommitted)
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
	open, err := tablesWithoutRowSecurity(ctx, tx)
	if err != nil {
		return err
	}
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

// tablesWithoutRowSecurity names the driver's tables that rowSecurityOffSQL
// finds. Its rows are closed when it returns, before the transaction runs its
// next statement.
func tablesWithoutRowSecurity(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, rowSecurityOffSQL, ObjectsTable, ChunksTable)
	if err != nil {
		return nil, fmt.Errorf("postgresblob: read row-level security: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var open []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("postgresblob: read row-level security: %w", err)
		}
		open = append(open, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresblob: read row-level security: %w", err)
	}
	return open, nil
}

type bucket struct {
	db         *sql.DB
	chunkSize  int   // bytes per chunk of a new object; ChunkSize outside tests
	shortLimit int64 // longest range read without a snapshot; ShortReadLimit outside tests
	firstFetch int64 // the least a reader's first piece asks for; firstFetch outside tests

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

// Attributes implements driver.Bucket. The ETag names the content the key
// holds, so it changes with every write and stays the same until the next.
func (b *bucket) Attributes(ctx context.Context, key string) (*driver.Attributes, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	obj, err := b.object(ctx, key)
	if err != nil {
		return nil, err
	}
	return &driver.Attributes{
		CacheControl:       obj.cacheControl,
		ContentDisposition: obj.contentDisposition,
		ContentEncoding:    obj.contentEncoding,
		ContentLanguage:    obj.contentLanguage,
		ContentType:        normalizeContentType(obj.contentType),
		Metadata:           obj.metadata,
		CreateTime:         obj.created,
		ModTime:            obj.modified,
		Size:               obj.size,
		MD5:                obj.md5,
		ETag:               `"` + obj.contentID + `"`,
	}, nil
}

type object struct {
	contentID          string
	contentType        string
	cacheControl       string
	contentDisposition string
	contentEncoding    string
	contentLanguage    string
	metadata           map[string]string
	md5                []byte
	size               int64
	chunkSize          int64
	created            time.Time
	modified           time.Time
}

func (b *bucket) object(ctx context.Context, key string) (object, error) {
	var (
		obj      object
		metadata string
	)
	err := b.db.QueryRowContext(ctx, objectSQL, key).Scan(&obj.contentID, &obj.contentType, &obj.cacheControl,
		&obj.contentDisposition, &obj.contentEncoding, &obj.contentLanguage, &metadata, &obj.md5,
		&obj.size, &obj.chunkSize, &obj.created, &obj.modified)
	if errors.Is(err, sql.ErrNoRows) {
		return object{}, notFound(key)
	}
	if err != nil {
		return object{}, fmt.Errorf("postgresblob: read %q: %w", key, err)
	}
	if obj.chunkSize <= 0 {
		return object{}, fmt.Errorf("postgresblob: %q has chunk size %d: %w", key, obj.chunkSize, gcerrors.ErrInternal)
	}
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &obj.metadata); err != nil {
			return object{}, fmt.Errorf("postgresblob: %q has unreadable metadata: %w: %w", key, err, gcerrors.ErrInternal)
		}
	}
	return obj, nil
}

// NewRangeReader implements driver.Bucket. It reads the key's row now and
// the bytes from the first Read on, so a conditional request that the server
// answers with 304 reads no content. opts.BeforeRead is called once the
// object is found, and not at all for a key that is absent, as the driver
// contract asks.
func (b *bucket) NewRangeReader(ctx context.Context, key string, offset, length int64, opts *driver.ReaderOptions) (driver.Reader, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%w: negative offset %d", gcerrors.ErrInvalidArgument, offset)
	}
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	obj, err := b.object(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := callBefore(opts.BeforeRead); err != nil {
		return nil, err
	}
	start := min(offset, obj.size)
	end := obj.size
	if length >= 0 && length < obj.size-start {
		end = start + length
	}
	fetch := b.firstFetch
	if fetch <= 0 {
		fetch = firstFetch
	}
	return &reader{
		db:        b.db,
		ctx:       ctx,
		key:       key,
		contentID: obj.contentID,
		chunkSize: obj.chunkSize,
		pos:       start,
		end:       end,
		snapshot:  end-start > b.shortLimit,
		fetch:     min(fetch, obj.chunkSize),
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
	snapshot  bool  // read every piece inside one REPEATABLE READ transaction
	fetch     int64 // the least the next piece asks for

	buf   []byte  // fetched and not yet returned
	tx    *sql.Tx // the snapshot of a long range, from its first piece to the end of the read
	begun bool    // the snapshot was begun, so a missing tx means it has ended
	err   error   // returned by every Read from now on
	attrs driver.ReaderAttributes
}

// querier is what a piece is read through: the pool, or a long range's
// snapshot.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Read implements io.Reader. It fills p across pieces and chunks, as the
// SDK's conformance suite expects of a driver, and ends a long range's
// snapshot as soon as the range's last byte is returned.
func (r *reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n := 0
	for n < len(p) {
		if len(r.buf) == 0 {
			if r.pos >= r.end {
				r.finish()
				if n > 0 {
					return n, nil
				}
				return 0, io.EOF
			}
			if err := r.fill(int64(len(p) - n)); err != nil {
				r.err = err
				r.finish()
				return n, err
			}
			continue
		}
		copied := copy(p[n:], r.buf)
		r.buf = r.buf[copied:]
		r.pos += int64(copied)
		n += copied
	}
	if r.pos >= r.end {
		r.finish()
	}
	return n, nil
}

// fill fetches the next piece: from the next byte to return, the more of
// need and r.fetch, but never past the end of its chunk or of the range.
func (r *reader) fill(need int64) error {
	q, err := r.source()
	if err != nil {
		return err
	}
	seq := r.pos / r.chunkSize
	from := r.pos - seq*r.chunkSize
	length := min(r.end-r.pos, r.chunkSize-from, max(need, r.fetch))
	var data []byte
	err = q.QueryRowContext(r.ctx, pieceSQL, r.key, r.contentID, seq, from+1, length).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && int64(len(data)) != length) {
		return r.gone(q, seq)
	}
	if err != nil {
		return fmt.Errorf("postgresblob: read %q: chunk %d: %w", r.key, seq, err)
	}
	r.buf = data
	r.fetch = min(2*r.fetch, r.chunkSize)
	return nil
}

// source is the pool for a short range. For a long one it is the snapshot,
// begun with the first piece. The snapshot sets
// idle_in_transaction_session_timeout for itself only (streamIdleTimeout).
func (r *reader) source() (querier, error) {
	if !r.snapshot {
		return r.db, nil
	}
	if r.tx != nil {
		return r.tx, nil
	}
	if r.begun {
		return nil, fmt.Errorf("postgresblob: read %q: its snapshot has already ended: %w", r.key, gcerrors.ErrInternal)
	}
	r.begun = true
	tx, err := r.db.BeginTx(r.ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("postgresblob: read %q: begin: %w", r.key, err)
	}
	if _, err := tx.ExecContext(r.ctx, `SELECT set_config('idle_in_transaction_session_timeout', $1, true)`,
		streamIdleTimeout); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("postgresblob: read %q: %w", r.key, err)
	}
	r.tx = tx
	return tx, nil
}

// gone explains a piece that is missing or shorter than asked for. If the key
// no longer holds the content the reader was opened on, a delete or overwrite
// committed in between: FailedPrecondition, and the caller may open the key
// again. If it still does, the content lacks part of its bytes: Internal.
func (r *reader) gone(q querier, seq int64) error {
	var current string
	err := q.QueryRowContext(r.ctx, contentIDSQL, r.key).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != r.contentID) {
		return fmt.Errorf("postgresblob: %q was replaced or deleted after the reader was opened; open it again: %w",
			r.key, gcerrors.ErrFailedPrecondition)
	}
	if err != nil {
		return fmt.Errorf("postgresblob: read %q: %w", r.key, err)
	}
	return fmt.Errorf("postgresblob: %q is missing part of its content at chunk %d: %w", r.key, seq, gcerrors.ErrInternal)
}

// finish ends the snapshot of a long range.
func (r *reader) finish() {
	if r.tx != nil {
		_ = r.tx.Commit()
		r.tx = nil
	}
}

// Close implements driver.Reader. A Read after Close returns errReaderClosed.
func (r *reader) Close() error {
	r.finish()
	r.buf = nil
	if r.err == nil {
		r.err = errReaderClosed
	}
	return nil
}

// Attributes implements driver.Reader.
func (r *reader) Attributes() *driver.ReaderAttributes { return &r.attrs }

// As implements driver.Reader.
func (*reader) As(any) bool { return false }

// NewTypedWriter implements driver.Bucket. Every attribute in opts is kept
// with the object; ContentMD5 is checked by the SDK, and the driver stores the
// MD5 of the bytes it was given, which Attributes returns.
func (b *bucket) NewTypedWriter(ctx context.Context, key, contentType string, opts *driver.WriterOptions) (driver.Writer, error) {
	if err := checkNewKey(key); err != nil {
		return nil, err
	}
	// The SDK checks the key and the metadata; these values go into text
	// columns too, which hold only UTF-8 without U+0000. Refusing them here
	// keeps a write that would fail at Close from sending any chunk.
	for name, value := range map[string]string{
		"content type": contentType, "Cache-Control": opts.CacheControl,
		"Content-Disposition": opts.ContentDisposition, "Content-Encoding": opts.ContentEncoding,
		"Content-Language": opts.ContentLanguage,
	} {
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("%w: postgresblob: the %s of %q is not valid UTF-8, which PostgreSQL text needs",
				gcerrors.ErrInvalidArgument, name, key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("%w: postgresblob: the %s of %q contains U+0000, which PostgreSQL text cannot hold",
				gcerrors.ErrInvalidArgument, name, key)
		}
	}
	metadata := ""
	if len(opts.Metadata) > 0 {
		encoded, err := json.Marshal(opts.Metadata)
		if err != nil {
			return nil, fmt.Errorf("%w: postgresblob: metadata of %q: %w", gcerrors.ErrInvalidArgument, key, err)
		}
		metadata = string(encoded)
	}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	if err := callBefore(opts.BeforeWrite); err != nil {
		return nil, err
	}
	return &writer{
		db:                 b.db,
		ctx:                ctx,
		key:                key,
		contentType:        normalizeContentType(contentType),
		cacheControl:       opts.CacheControl,
		contentDisposition: opts.ContentDisposition,
		contentEncoding:    opts.ContentEncoding,
		contentLanguage:    opts.ContentLanguage,
		metadata:           metadata,
		ifNotExist:         opts.IfNotExist,
		contentID:          uuid.NewString(),
		chunkSize:          b.chunkSize,
		md5:                md5.New(),
	}, nil
}

type writer struct {
	db                 *sql.DB
	ctx                context.Context
	key                string
	contentType        string
	cacheControl       string
	contentDisposition string
	contentEncoding    string
	contentLanguage    string
	metadata           string // JSON, or "" for none
	ifNotExist         bool
	contentID          string
	chunkSize          int

	tx     *sql.Tx // begun with the first chunk; holds every chunk until Close commits
	buf    []byte  // the next chunk, not yet full
	md5    hash.Hash
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
	// A write that fails part way fails the whole object, so hashing all of p
	// up front never records bytes that a committed object lacks.
	_, _ = w.md5.Write(p)
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
	tx, err := w.db.BeginTx(w.ctx, readCommitted)
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
	args := []any{w.key, w.contentID, w.contentType, w.cacheControl, w.contentDisposition, w.contentEncoding,
		w.contentLanguage, w.metadata, w.md5.Sum(nil), w.size, w.chunkSize}
	if w.ifNotExist {
		res, err := w.tx.ExecContext(w.ctx, insertObjectSQL, args...)
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
	if _, err := w.tx.ExecContext(w.ctx, upsertObjectSQL, args...); err != nil {
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
	if err := checkKey(key); err != nil {
		return err
	}
	if err := b.ensure(ctx); err != nil {
		return err
	}
	if err := callBefore(opts.BeforeDelete); err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, readCommitted)
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
// moving the bytes through the client, and the copy keeps every attribute of
// the source.
func (b *bucket) Copy(ctx context.Context, dstKey, srcKey string, opts *driver.CopyOptions) error {
	if err := checkNewKey(dstKey); err != nil {
		return err
	}
	if err := checkKey(srcKey); err != nil {
		return err
	}
	if err := b.ensure(ctx); err != nil {
		return err
	}
	if err := callBefore(opts.BeforeCopy); err != nil {
		return err
	}
	contentID := uuid.NewString()
	tx, err := b.db.BeginTx(ctx, readCommitted)
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
// pages the resulting entries. Homebox lists only through the SDK's io/fs
// Open, which its thumbnail step uses: for a key that does not exist, Open
// lists to see whether the name is a directory.
func (b *bucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	if err := callBefore(opts.BeforeList); err != nil {
		return nil, err
	}
	if strings.IndexByte(opts.Prefix, 0) >= 0 {
		// No key holds U+0000 (checkKey), so no key has this prefix.
		return &driver.ListPage{}, nil
	}
	// A page token is the last key or directory of the page before, so it is
	// UTF-8 without U+0000; anything else did not come from this driver.
	if token := string(opts.PageToken); !utf8.ValidString(token) || strings.IndexByte(token, 0) >= 0 {
		return nil, fmt.Errorf("%w: postgresblob: bad page token %q", gcerrors.ErrInvalidArgument, opts.PageToken)
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
		object, err := scanListObject(rows)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
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

// listDelimited pages by entry, a key or a directory, in byte order. The page
// token is the last entry of the page before, as in the flat listing, so a
// write or delete between two pages never repeats, reorders or skips an
// entry that was there throughout. Collapsing keys into their directories
// keeps byte order, and the keys of one directory are next to each other, so
// an entry is a repeat exactly when it equals the one before it.
func (b *bucket) listDelimited(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	// A key at or before the token belongs to an entry at or before it, since
	// an entry never sorts after its keys; the query leaves those out.
	after := string(opts.PageToken)
	rows, err := b.db.QueryContext(ctx, listAfterSQL, opts.Prefix, after)
	if err != nil {
		return nil, fmt.Errorf("postgresblob: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]*driver.ListObject, 0)
	var next []byte
	for rows.Next() {
		entry, err := scanListObject(rows)
		if err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(entry.Key, opts.Prefix)
		if i := strings.Index(rest, opts.Delimiter); i >= 0 {
			entry = &driver.ListObject{Key: opts.Prefix + rest[:i+len(opts.Delimiter)], IsDir: true}
		}
		// The directory the page before ended with, or one already on this page.
		if entry.Key <= after || (len(entries) > 0 && entries[len(entries)-1].Key == entry.Key) {
			continue
		}
		if len(entries) == pageSize {
			next = []byte(entries[len(entries)-1].Key)
			break
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresblob: list: %w", err)
	}
	return &driver.ListPage{Objects: entries, NextPageToken: next}, nil
}

// scanListObject reads one row of listFlatSQL or listAfterSQL.
func scanListObject(rows *sql.Rows) (*driver.ListObject, error) {
	var object driver.ListObject
	if err := rows.Scan(&object.Key, &object.Size, &object.ModTime, &object.MD5); err != nil {
		return nil, fmt.Errorf("postgresblob: list: scan: %w", err)
	}
	return &object, nil
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

// checkKey refuses a key that contains U+0000. The SDK has already refused a
// key that is not UTF-8; U+0000 is the one character left that a PostgreSQL
// text value cannot hold. Homebox's keys are a group id and a hex hash.
func checkKey(key string) error {
	if strings.IndexByte(key, 0) >= 0 {
		return fmt.Errorf("%w: postgresblob: key %q contains U+0000, which PostgreSQL text cannot hold",
			gcerrors.ErrInvalidArgument, key)
	}
	return nil
}

// checkNewKey is checkKey for a key an object is written to, which must also
// not be empty, as the SDK's conformance suite expects of every driver.
func checkNewKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: postgresblob: an object needs a non-empty key", gcerrors.ErrInvalidArgument)
	}
	return checkKey(key)
}

// Compile-time checks that the driver types satisfy the gocloud interfaces.
var (
	_ io.WriteCloser = (*writer)(nil)
	_ driver.Reader  = (*reader)(nil)
	_ driver.Bucket  = (*bucket)(nil)
)
