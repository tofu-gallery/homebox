// Tofu (2026-11-05): this package is Tofu's addition; it is not part of upstream Homebox.
//
// Package postgresblob implements a gocloud.dev/blob driver that keeps every
// object in a PostgreSQL table, and the wiring Homebox uses to select it.
//
// Tofu's container hosting scales an app to zero and gives a new container a
// fresh disk on every start, so Homebox's default fileblob bucket under /data
// loses item photos and attachments on every restart and deploy. A copy's
// database survives both. This driver puts the bytes in that database, in a
// table of its own (tofu_blobs), and leaves every existing table alone.
//
// The URL scheme is "postgresblob". OpenBucketURL uses the process-wide pool
// registered with RegisterPool, so no second connection pool is opened:
//
//	postgresblob://
//
// The table is created on first use (RegisterPool creates it at startup), with
// IF NOT EXISTS and row-level security on. Objects are written with one
// INSERT ... ON CONFLICT statement at Close, so a reader never sees a
// half-written object and a failed write leaves nothing behind. Reads copy the
// requested range into memory before returning, so a concurrent Delete cannot
// affect a reader and no transaction is held open while a caller streams.
package postgresblob

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/gcerrors"
)

const (
	// Scheme is the URL scheme this package registers with gocloud.dev/blob.
	Scheme = "postgresblob"

	// TableName is the table that holds every object. It is Tofu's own table;
	// no existing table is read or changed.
	TableName = "tofu_blobs"

	// MaxBlobSize caps one object at 100 MiB. Homebox caps an ordinary upload
	// at 10 MiB by default (HBOX_WEB_MAX_FILE_UPLOAD, 10), and its thumbnail
	// path already refuses to read an original over 100 MiB. The driver has to
	// hold a whole object in memory before PostgreSQL receives it as one bytea
	// parameter, so this is the app's own largest blob ceiling, not a new one.
	// A generated export ZIP over 100 MiB is the one object the app can build
	// past this; it is refused rather than risking the process's memory.
	MaxBlobSize = 100 << 20

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
	createTableSQL = `
CREATE TABLE IF NOT EXISTS ` + TableName + ` (
	"key" text PRIMARY KEY,
	content bytea NOT NULL,
	content_type text NOT NULL,
	size bigint NOT NULL CHECK (size >= 0),
	created_at timestamptz NOT NULL DEFAULT now(),
	mod_time timestamptz NOT NULL DEFAULT now()
)`

	attributesSQL = `SELECT content_type, size, created_at, mod_time FROM ` + TableName + ` WHERE "key" = $1`

	// $2 is the byte offset and $3 the byte count; a negative count means
	// "until the end". substring(bytea, int, int) is 1-based, so offset 0 is
	// start 1. A range that starts past the end returns an empty bytea.
	rangeReaderSQL = `SELECT content_type, size, mod_time,
	CASE WHEN $3::bigint < 0 THEN substring(content from $2::int + 1)
	     ELSE substring(content from $2::int + 1 for $3::int) END
	FROM ` + TableName + ` WHERE "key" = $1`

	upsertSQL = `INSERT INTO ` + TableName + ` ("key", content, content_type, size, created_at, mod_time)
VALUES ($1, $2, $3, $4, now(), now())
ON CONFLICT ("key") DO UPDATE SET
	content = EXCLUDED.content,
	content_type = EXCLUDED.content_type,
	size = EXCLUDED.size,
	mod_time = EXCLUDED.mod_time`

	insertIfNotExistSQL = `INSERT INTO ` + TableName + ` ("key", content, content_type, size, created_at, mod_time)
VALUES ($1, $2, $3, $4, now(), now())
ON CONFLICT ("key") DO NOTHING`

	deleteSQL = `DELETE FROM ` + TableName + ` WHERE "key" = $1`

	copySQL = `INSERT INTO ` + TableName + ` ("key", content, content_type, size, created_at, mod_time)
SELECT $1, content, content_type, size, now(), now()
FROM ` + TableName + ` WHERE "key" = $2
ON CONFLICT ("key") DO UPDATE SET
	content = EXCLUDED.content,
	content_type = EXCLUDED.content_type,
	size = EXCLUDED.size,
	mod_time = EXCLUDED.mod_time`

	// COLLATE "C" makes ORDER BY and range comparisons use UTF-8 byte order,
	// which is what the blob driver contract requires; a database's default
	// collation may order differently.
	listFlatSQL = `SELECT "key", size, mod_time FROM ` + TableName + `
WHERE starts_with("key", $1) AND ($2::text = '' OR "key" COLLATE "C" > $2 COLLATE "C")
ORDER BY "key" COLLATE "C"
LIMIT $3`

	listAllSQL = `SELECT "key", size, mod_time FROM ` + TableName + `
WHERE starts_with("key", $1)
ORDER BY "key" COLLATE "C"`
)

var (
	// errNoPool is returned when a bucket is opened through the URL scheme
	// before the application has registered its database pool.
	errNoPool = errors.New("postgresblob: no PostgreSQL pool registered; call RegisterPool at startup")

	// errTooLarge is returned when one object exceeds MaxBlobSize.
	errTooLarge = fmt.Errorf("postgresblob: object exceeds the %d MiB limit", MaxBlobSize>>20)

	// errWriterClosed is returned when Write or Close is called after Close.
	errWriterClosed = errors.New("postgresblob: writer is already closed")

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
// creates the table if it does not exist yet. The pool stays owned by the
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
	return newReadyBucket(db), nil
}

// NewBucket returns a bucket on db that creates the table on its first
// operation. The application uses the URL scheme instead, so that the pool is
// registered in one place; this constructor exists for tests and for callers
// that already hold a pool.
func NewBucket(db *sql.DB) *blob.Bucket {
	return blob.NewBucket(&bucket{db: db})
}

func newReadyBucket(db *sql.DB) *blob.Bucket {
	return blob.NewBucket(&bucket{db: db, schemaReady: true})
}

// ensureSchema creates the table once per pool and turns row-level security
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
	if _, err := tx.ExecContext(ctx, createTableSQL); err != nil {
		return fmt.Errorf("postgresblob: create %s: %w", TableName, err)
	}
	// The app's role owns the table and is not limited by row security, while
	// the database's Data API roles see no row. The schema step does this for
	// every table that exists when a start begins; this table is created after
	// it, so it does it here.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+TableName+` ENABLE ROW LEVEL SECURITY`); err != nil {
		return fmt.Errorf("postgresblob: enable row-level security on %s: %w", TableName, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresblob: commit schema: %w", err)
	}
	return nil
}

type bucket struct {
	db          *sql.DB
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
	case errors.Is(err, gcerrors.ErrInvalidArgument), errors.Is(err, errTooLarge):
		return gcerrors.InvalidArgument
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
	var (
		contentType string
		size        int64
		created     time.Time
		modified    time.Time
	)
	err := b.db.QueryRowContext(ctx, attributesSQL, key).Scan(&contentType, &size, &created, &modified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(key)
	}
	if err != nil {
		return nil, fmt.Errorf("postgresblob: attributes %q: %w", key, err)
	}
	return &driver.Attributes{
		ContentType: normalizeContentType(contentType),
		Size:        size,
		CreateTime:  created,
		ModTime:     modified,
	}, nil
}

// NewRangeReader implements driver.Bucket.
func (b *bucket) NewRangeReader(ctx context.Context, key string, offset, length int64, opts *driver.ReaderOptions) (driver.Reader, error) {
	if err := callBefore(opts.BeforeRead); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("%w: negative offset %d", gcerrors.ErrInvalidArgument, offset)
	}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	var (
		contentType string
		size        int64
		modified    time.Time
		data        []byte
	)
	err := b.db.QueryRowContext(ctx, rangeReaderSQL, key, offset, length).
		Scan(&contentType, &size, &modified, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(key)
	}
	if err != nil {
		return nil, fmt.Errorf("postgresblob: read %q: %w", key, err)
	}
	return &reader{
		Reader: bytes.NewReader(data),
		attrs: driver.ReaderAttributes{
			ContentType: normalizeContentType(contentType),
			ModTime:     modified,
			Size:        size,
		},
	}, nil
}

type reader struct {
	*bytes.Reader
	attrs driver.ReaderAttributes
}

// Close implements driver.Reader. The bytes were copied out of the database
// when the reader was opened, so there is nothing to release.
func (r *reader) Close() error { return nil }

// Attributes implements driver.Reader.
func (r *reader) Attributes() *driver.ReaderAttributes { return &r.attrs }

// As implements driver.Reader.
func (*reader) As(any) bool { return false }

// NewTypedWriter implements driver.Bucket. The writer buffers the object and
// writes it with one statement at Close, so a failed write leaves nothing.
func (b *bucket) NewTypedWriter(ctx context.Context, key, contentType string, opts *driver.WriterOptions) (driver.Writer, error) {
	if err := callBefore(opts.BeforeWrite); err != nil {
		return nil, err
	}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	return &writer{
		bucket:      b,
		ctx:         ctx,
		key:         key,
		contentType: normalizeContentType(contentType),
		ifNotExist:  opts.IfNotExist,
	}, nil
}

type writer struct {
	bucket      *bucket
	ctx         context.Context
	key         string
	contentType string
	ifNotExist  bool
	buf         bytes.Buffer
	writeErr    error
	closed      bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errWriterClosed
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.buf.Len()+len(p) > MaxBlobSize {
		w.writeErr = errTooLarge
		return 0, w.writeErr
	}
	return w.buf.Write(p)
}

func (w *writer) Close() error {
	if w.closed {
		return errWriterClosed
	}
	w.closed = true
	if w.writeErr != nil {
		return w.writeErr
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}

	query := upsertSQL
	if w.ifNotExist {
		query = insertIfNotExistSQL
	}
	res, err := w.bucket.db.ExecContext(w.ctx, query, w.key, w.buf.Bytes(), w.contentType, int64(w.buf.Len()))
	if err != nil {
		return fmt.Errorf("postgresblob: write %q: %w", w.key, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgresblob: write %q: rows affected: %w", w.key, err)
	}
	if affected == 0 {
		if w.ifNotExist {
			return fmt.Errorf("postgresblob: object %q already exists: %w", w.key, gcerrors.ErrFailedPrecondition)
		}
		return fmt.Errorf("postgresblob: write %q affected no row: %w", w.key, gcerrors.ErrInternal)
	}
	w.buf.Reset()
	return nil
}

// Delete implements driver.Bucket.
func (b *bucket) Delete(ctx context.Context, key string, opts *driver.DeleteOptions) error {
	if err := callBefore(opts.BeforeDelete); err != nil {
		return err
	}
	if err := b.ensure(ctx); err != nil {
		return err
	}
	res, err := b.db.ExecContext(ctx, deleteSQL, key)
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
	return nil
}

// Copy implements driver.Bucket. It copies inside the database, without
// moving the bytes through the client.
func (b *bucket) Copy(ctx context.Context, dstKey, srcKey string, opts *driver.CopyOptions) error {
	if err := callBefore(opts.BeforeCopy); err != nil {
		return err
	}
	if err := b.ensure(ctx); err != nil {
		return err
	}
	res, err := b.db.ExecContext(ctx, copySQL, dstKey, srcKey)
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
	return nil
}

// ListPaged implements driver.Bucket. Without a delimiter it pages by key in
// byte order; with one it collapses keys below a directory into one entry and
// pages the resulting entries. Homebox itself does not list the bucket; this
// exists because the driver interface requires it.
func (b *bucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	if err := callBefore(opts.BeforeList); err != nil {
		return nil, err
	}
	if err := b.ensure(ctx); err != nil {
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
