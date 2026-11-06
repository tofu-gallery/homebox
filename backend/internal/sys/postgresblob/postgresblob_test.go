// Tofu (2026-11-05): tests for Tofu's PostgreSQL blob driver. The database
// tests run only when TEST_DATABASE_URL points at a throwaway PostgreSQL
// database; without it they skip, as the rest of this repository's
// database-backed tests do.
package postgresblob

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

var tables = []string{ObjectsTable, ChunksTable}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL blob driver tests")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(context.Background()))
	return db
}

// dsnWith is TEST_DATABASE_URL with one more connection parameter; pgx sends
// a parameter it does not know to the server as a setting for the session.
func dsnWith(t *testing.T, param string) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL blob driver tests")
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&" + param
	}
	return dsn + "?" + param
}

// countingDB opens a pool on the test database that counts every byte
// PostgreSQL sends it, so a test can tell how much a read fetched.
func countingDB(t *testing.T) (*sql.DB, *atomic.Int64) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL blob driver tests")
	}
	config, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	received := new(atomic.Int64)
	dial := config.DialFunc
	config.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &countingConn{Conn: conn, received: received}, nil
	}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(context.Background()))
	return db, received
}

type countingConn struct {
	net.Conn
	received *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.received.Add(int64(n))
	return n, err
}

// reset drops the driver's tables so each test starts from a known state and
// can observe the tables being created again.
func reset(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range tables {
		_, err := db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table)
		require.NoError(t, err)
	}
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists)
	require.NoError(t, err)
	return exists
}

func rowSecurity(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var on bool
	err := db.QueryRowContext(context.Background(),
		`SELECT relrowsecurity FROM pg_class WHERE oid = to_regclass($1)`, table).Scan(&on)
	require.NoError(t, err)
	return on
}

func newBucket(t *testing.T, db *sql.DB) *blob.Bucket {
	t.Helper()
	b := NewBucket(db)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// newSmallBucket writes 7-byte chunks, reads a range longer than 20 bytes
// inside a snapshot and begins every read with a 3-byte piece, so a test
// object of a few dozen bytes takes the same paths that a backup of several
// hundred megabytes takes with the real sizes.
func newSmallBucket(t *testing.T, db *sql.DB) *blob.Bucket {
	t.Helper()
	b := blob.NewBucket(&bucket{db: db, chunkSize: 7, shortLimit: 20, firstFetch: 3})
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// buckets runs fn against both sizes; small says which one it got.
func buckets(t *testing.T, db *sql.DB, fn func(t *testing.T, b *blob.Bucket, small bool)) {
	t.Run("real sizes", func(t *testing.T) { fn(t, newBucket(t, db), false) })
	t.Run("small chunks", func(t *testing.T) { fn(t, newSmallBucket(t, db), true) })
}

func mustRead(t *testing.T, b *blob.Bucket, key string) []byte {
	t.Helper()
	data, err := b.ReadAll(context.Background(), key)
	require.NoError(t, err)
	return data
}

func pattern(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	return data
}

// noise is incompressible, so PostgreSQL stores it as it is rather than
// compressing it away.
func noise(seed int64, size int) []byte {
	data := make([]byte, size)
	_, _ = rand.New(rand.NewSource(seed)).Read(data)
	return data
}

// chunkRows reports how many chunk rows a key has, how many bytes they hold
// and how many contents they belong to.
func chunkRows(t *testing.T, db *sql.DB, key string) (rows int, size int64, contents int) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*), coalesce(sum(octet_length(data)), 0), count(DISTINCT content_id) FROM `+ChunksTable+
			` WHERE "key" = $1`, key).Scan(&rows, &size, &contents)
	require.NoError(t, err)
	return rows, size, contents
}

func TestSchemeRegistered(t *testing.T) {
	RegisterScheme()
	assert.True(t, blob.DefaultURLMux().ValidBucketScheme(Scheme))
}

func TestTablesCreatedOnFirstUse(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	for _, table := range tables {
		require.False(t, tableExists(t, db, table), "%s exists before the driver is used", table)
	}

	b := newBucket(t, db)
	for _, table := range tables {
		require.False(t, tableExists(t, db, table), "NewBucket should not touch the database")
	}

	require.NoError(t, b.WriteAll(context.Background(), "first", []byte("x"), nil))
	for _, table := range tables {
		assert.True(t, tableExists(t, db, table), "%s was not created by the first write", table)
		assert.True(t, rowSecurity(t, db, table), "%s was created without row-level security", table)
	}
}

func TestStartWhileAnotherInstanceReads(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, db))

	// An instance that is serving holds ACCESS SHARE on the tables while it
	// reads. A later start must not wait for it: ALTER TABLE would need ACCESS
	// EXCLUSIVE, and every attachment read and write would queue behind it.
	serving, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = serving.Rollback() }()
	for _, table := range tables {
		_, err = serving.ExecContext(ctx, `SELECT count(*) FROM `+table)
		require.NoError(t, err)
	}

	startCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	require.NoError(t, ensureSchema(startCtx, db), "a second start waited on an instance that was reading")
	for _, table := range tables {
		assert.True(t, rowSecurity(t, db, table))
	}
}

func TestConcurrentStartsCreateTheTablesOnce(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Tofu starts several copies of a new deployment at once. Without the
	// advisory lock, CREATE TABLE IF NOT EXISTS from two of them can still
	// collide in PostgreSQL's catalog (a duplicate key in pg_type).
	const starts = 8
	for round := 0; round < 10; round++ {
		reset(t, db)
		begin := make(chan struct{})
		errs := make(chan error, starts)
		var wg sync.WaitGroup
		for i := 0; i < starts; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-begin
				errs <- ensureSchema(ctx, db)
			}()
		}
		close(begin)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
		for _, table := range tables {
			require.True(t, tableExists(t, db, table))
			require.True(t, rowSecurity(t, db, table))
		}
	}
}

func TestRoundTripSmallAndLarge(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	t.Run("small", func(t *testing.T) {
		want := []byte("a small attachment")
		require.NoError(t, b.WriteAll(ctx, "small", want, &blob.WriterOptions{ContentType: "text/plain"}))
		assert.Equal(t, want, mustRead(t, b, "small"))

		attrs, err := b.Attributes(ctx, "small")
		require.NoError(t, err)
		assert.EqualValues(t, len(want), attrs.Size)
		assert.Equal(t, "text/plain", attrs.ContentType)

		rows, size, _ := chunkRows(t, db, "small")
		assert.Equal(t, 1, rows)
		assert.EqualValues(t, len(want), size)
	})

	t.Run("large", func(t *testing.T) {
		want := noise(1, 9<<20+17) // three chunks: 4 MiB, 4 MiB and the rest
		require.NoError(t, b.WriteAll(ctx, "large", want, &blob.WriterOptions{ContentType: "application/octet-stream"}))
		assert.Equal(t, want, mustRead(t, b, "large"))

		rows, size, contents := chunkRows(t, db, "large")
		assert.Equal(t, 3, rows)
		assert.EqualValues(t, len(want), size)
		assert.Equal(t, 1, contents)
	})

	t.Run("empty", func(t *testing.T) {
		// Homebox can write a zero-byte attachment (for example an empty file
		// inside an imported ZIP), and the SDK's own conformance test expects
		// "no write then close" to make an empty object.
		require.NoError(t, b.WriteAll(ctx, "empty", nil, nil))
		got, err := b.ReadAll(ctx, "empty")
		require.NoError(t, err)
		assert.Empty(t, got)

		attrs, err := b.Attributes(ctx, "empty")
		require.NoError(t, err)
		assert.EqualValues(t, 0, attrs.Size)

		rows, _, _ := chunkRows(t, db, "empty")
		assert.Equal(t, 0, rows)
	})

	t.Run("survives a new pool", func(t *testing.T) {
		want := noise(2, 3<<20)
		require.NoError(t, b.WriteAll(ctx, "restart", want, nil))

		// A new process would build a new pool; nothing is cached in the
		// driver that the bytes depend on.
		db2, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db2.Close() })
		b2 := newBucket(t, db2)
		assert.Equal(t, want, mustRead(t, b2, "restart"))
	})
}

func TestLargeObjectIsChunked(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 120 MiB")
	}
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	// Larger than any one bytea value may usefully be held in memory, and
	// written the way Homebox writes a backup: io.Copy from a file, so the
	// driver sees small writes and holds one chunk at a time.
	const key, size, seed = "group/exports/backup.zip", 120 << 20, 42
	want := sha256.New()
	w, err := b.NewWriter(ctx, key, &blob.WriterOptions{ContentType: "application/zip"})
	require.NoError(t, err)
	n, err := io.Copy(w, io.TeeReader(io.LimitReader(rand.New(rand.NewSource(seed)), size), want))
	require.NoError(t, err)
	require.EqualValues(t, size, n)
	require.NoError(t, w.Close())

	var rows int
	var largest int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*), max(octet_length(data)) FROM `+ChunksTable+` WHERE "key" = $1`, key).Scan(&rows, &largest))
	assert.Equal(t, size/ChunkSize, rows)
	assert.EqualValues(t, ChunkSize, largest)

	// The whole object is longer than ShortReadLimit, so it is read inside a
	// snapshot.
	r, err := b.NewReader(ctx, key, nil)
	require.NoError(t, err)
	got := sha256.New()
	n, err = io.Copy(got, r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.EqualValues(t, size, n)
	assert.Equal(t, want.Sum(nil), got.Sum(nil))

	// A short range across the boundary of the first two chunks.
	const off, length = ChunkSize - 10, 20
	src := rand.New(rand.NewSource(seed))
	_, err = io.CopyN(io.Discard, src, off)
	require.NoError(t, err)
	expect := make([]byte, length)
	_, err = io.ReadFull(src, expect)
	require.NoError(t, err)
	rr, err := b.NewRangeReader(ctx, key, off, length, nil)
	require.NoError(t, err)
	gotRange, err := io.ReadAll(rr)
	require.NoError(t, err)
	require.NoError(t, rr.Close())
	assert.Equal(t, expect, gotRange)
}

func TestRangeRead(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	// 36 bytes: five 7-byte chunks and a 1-byte one in the small bucket.
	const content = "0123456789abcdefghijklmnopqrstuvwxyz"
	tests := []struct {
		name   string
		offset int64
		length int64
		want   string
	}{
		{name: "start", offset: 0, length: 4, want: "0123"},
		{name: "middle", offset: 3, length: 4, want: "3456"},
		{name: "across chunks", offset: 5, length: 10, want: content[5:15]},
		{name: "to end", offset: 30, length: -1, want: content[30:]},
		{name: "whole", offset: 0, length: -1, want: content},
		{name: "long middle", offset: 2, length: 30, want: content[2:32]},
		{name: "negative length not -1", offset: 1, length: -42, want: content[1:]},
		{name: "zero length", offset: 4, length: 0, want: ""},
		{name: "at end", offset: 36, length: -1, want: ""},
		{name: "past end", offset: 50, length: 5, want: ""},
		{name: "offset past int4", offset: 1 << 40, length: -1, want: ""},
		{name: "length past int4", offset: 2, length: math.MaxInt64, want: content[2:]},
	}
	buckets(t, db, func(t *testing.T, b *blob.Bucket, _ bool) {
		require.NoError(t, b.WriteAll(ctx, "range", []byte(content), &blob.WriterOptions{ContentType: "text/plain"}))
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				r, err := b.NewRangeReader(ctx, "range", tt.offset, tt.length, nil)
				require.NoError(t, err)
				defer func() { _ = r.Close() }()
				assert.EqualValues(t, len(content), r.Size(), "a range reader reports the whole object's size")

				got, err := io.ReadAll(r)
				require.NoError(t, err)
				assert.Equal(t, tt.want, string(got))
			})
		}

		_, err := b.NewRangeReader(ctx, "absent", 0, -1, nil)
		require.Error(t, err)
		assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
	})
}

func TestList(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const prefix = "group/"
	keys := []string{prefix + "a/1", prefix + "a/2", prefix + "a/3", prefix + "b/1", prefix + "c"}
	for _, key := range keys {
		require.NoError(t, b.WriteAll(ctx, key, []byte(key), nil))
	}
	// A key outside the prefix must not appear.
	require.NoError(t, b.WriteAll(ctx, "other", []byte("other"), nil))

	t.Run("flat", func(t *testing.T) {
		var got []string
		token := blob.FirstPageToken
		for {
			objects, next, err := b.ListPage(ctx, token, 2, &blob.ListOptions{Prefix: prefix})
			require.NoError(t, err)
			for _, object := range objects {
				got = append(got, object.Key)
			}
			if len(next) == 0 {
				break
			}
			token = next
		}
		assert.Equal(t, keys, got)
	})

	t.Run("delimiter", func(t *testing.T) {
		var got []string
		var dirs []bool
		token := blob.FirstPageToken
		for {
			objects, next, err := b.ListPage(ctx, token, 2, &blob.ListOptions{Prefix: prefix, Delimiter: "/"})
			require.NoError(t, err)
			for _, object := range objects {
				got = append(got, object.Key)
				dirs = append(dirs, object.IsDir)
			}
			if len(next) == 0 {
				break
			}
			token = next
		}
		assert.Equal(t, []string{prefix + "a/", prefix + "b/", prefix + "c"}, got)
		assert.Equal(t, []bool{true, true, false}, dirs)
	})
}

func TestDelete(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newSmallBucket(t, db)
	ctx := context.Background()

	require.NoError(t, b.WriteAll(ctx, "delete-me", pattern(30), nil))
	rows, _, _ := chunkRows(t, db, "delete-me")
	require.Equal(t, 5, rows)
	require.NoError(t, b.Delete(ctx, "delete-me"))

	_, err := b.ReadAll(ctx, "delete-me")
	require.Error(t, err)
	assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
	rows, _, _ = chunkRows(t, db, "delete-me")
	assert.Equal(t, 0, rows, "a delete left chunks behind")

	err = b.Delete(ctx, "delete-me")
	require.Error(t, err)
	assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
}

func TestOverwriteKeepsOnlyTheNewChunks(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newSmallBucket(t, db)
	ctx := context.Background()

	require.NoError(t, b.WriteAll(ctx, "photo", pattern(50), nil))
	rows, _, _ := chunkRows(t, db, "photo")
	require.Equal(t, 8, rows)

	want := []byte("0123456789")
	require.NoError(t, b.WriteAll(ctx, "photo", want, nil))
	assert.Equal(t, want, mustRead(t, b, "photo"))
	rows, size, contents := chunkRows(t, db, "photo")
	assert.Equal(t, 2, rows, "the replaced content's chunks were left behind")
	assert.EqualValues(t, len(want), size)
	assert.Equal(t, 1, contents)
}

func TestFailedWriteLeavesNothing(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newSmallBucket(t, db)
	ctx := context.Background()

	old := []byte("the old content")
	require.NoError(t, b.WriteAll(ctx, "kept", old, nil))

	for _, key := range []string{"kept", "never-written"} {
		t.Run(key, func(t *testing.T) {
			before, _, _ := chunkRows(t, db, key)
			writeCtx, cancel := context.WithCancel(ctx)
			w, err := b.NewWriter(writeCtx, key, &blob.WriterOptions{ContentType: "text/plain"})
			require.NoError(t, err)
			// Four whole chunks go to the database inside the writer's
			// transaction before it is cancelled.
			_, err = w.Write(bytes.Repeat([]byte("n"), 30))
			require.NoError(t, err)

			cancel()
			err = w.Close()
			require.Error(t, err)
			assert.Equal(t, gcerrors.Canceled, gcerrors.Code(err))

			after, _, _ := chunkRows(t, db, key)
			assert.Equal(t, before, after, "a cancelled write left chunks behind")
			if key == "kept" {
				assert.Equal(t, old, mustRead(t, b, key))
			} else {
				_, err := b.Attributes(ctx, key)
				assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
			}
		})
	}
}

func TestConcurrentWriters(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const keyA, keyB = "concurrent/a", "concurrent/b"
	wantA, wantB := noise(3, 5<<20), noise(4, 5<<20+9)
	start := make(chan struct{})
	errs := make(chan error, 2)

	write := func(key string, data []byte) {
		<-start
		errs <- b.WriteAll(ctx, key, data, &blob.WriterOptions{ContentType: "application/octet-stream"})
	}
	go write(keyA, wantA)
	go write(keyB, wantB)
	close(start)

	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	assert.Equal(t, wantA, mustRead(t, b, keyA))
	assert.Equal(t, wantB, mustRead(t, b, keyB))
}

func TestConcurrentWritersSameKey(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newSmallBucket(t, db)
	ctx := context.Background()

	const key, writers = "same", 6
	contents := make([][]byte, writers)
	for i := range contents {
		contents[i] = bytes.Repeat([]byte{byte('a' + i)}, 40+i)
	}
	start := make(chan struct{})
	errs := make(chan error, writers)
	for i := range contents {
		go func(data []byte) {
			<-start
			errs <- b.WriteAll(ctx, key, data, nil)
		}(contents[i])
	}
	close(start)
	for range contents {
		require.NoError(t, <-errs)
	}

	got := mustRead(t, b, key)
	assert.Contains(t, contents, got, "the object is not one of the writes, whole")
	rows, size, ids := chunkRows(t, db, key)
	assert.Equal(t, (len(got)+6)/7, rows, "a write that lost the race left its chunks behind")
	assert.EqualValues(t, len(got), size)
	assert.Equal(t, 1, ids)
}

func TestReaderDuringWrite(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newSmallBucket(t, db)
	ctx := context.Background()

	const key = "in-flight"
	oldContent := bytes.Repeat([]byte("o"), 30)
	newContent := bytes.Repeat([]byte("n"), 60)
	require.NoError(t, b.WriteAll(ctx, key, oldContent, nil))

	w, err := b.NewWriter(ctx, key, &blob.WriterOptions{ContentType: "application/octet-stream"})
	require.NoError(t, err)

	// The new chunks are in the database but not committed: a reader sees the
	// old object whole, never a prefix of the new one.
	_, err = w.Write(newContent[:30])
	require.NoError(t, err)
	assert.Equal(t, oldContent, mustRead(t, b, key))

	_, err = w.Write(newContent[30:])
	require.NoError(t, err)
	assert.Equal(t, oldContent, mustRead(t, b, key))
	attrs, err := b.Attributes(ctx, key)
	require.NoError(t, err)
	assert.EqualValues(t, len(oldContent), attrs.Size)

	require.NoError(t, w.Close())
	assert.Equal(t, newContent, mustRead(t, b, key))
}

func TestReaderWhileWriterCloses(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	buckets(t, db, func(t *testing.T, b *blob.Bucket, small bool) {
		const key = "racing"
		oldContent, newContent := pattern(1<<20), noise(5, 1<<20+3)
		if small {
			// Longer than the small bucket's 20-byte limit for a read without
			// a snapshot, so these are read inside one.
			oldContent, newContent = pattern(60), bytes.Repeat([]byte("n"), 63)
		}
		require.NoError(t, b.WriteAll(ctx, key, oldContent, nil))

		w, err := b.NewWriter(ctx, key, &blob.WriterOptions{ContentType: "application/octet-stream"})
		require.NoError(t, err)
		_, err = w.Write(newContent)
		require.NoError(t, err)

		// Read in a loop while the writer commits. Every read must be one of
		// the two objects, whole; a read the commit lands in may instead fail
		// with FailedPrecondition. A torn read would be a different length or
		// different bytes.
		stop := make(chan struct{})
		running := make(chan struct{})
		bad := make(chan string, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			once := sync.Once{}
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := b.ReadAll(ctx, key)
				once.Do(func() { close(running) })
				if err != nil {
					if gcerrors.Code(err) == gcerrors.FailedPrecondition {
						continue
					}
					bad <- fmt.Sprintf("read error: %v", err)
					return
				}
				if !bytes.Equal(got, oldContent) && !bytes.Equal(got, newContent) {
					bad <- fmt.Sprintf("torn read: got %d bytes", len(got))
					return
				}
			}
		}()

		<-running
		require.NoError(t, w.Close())
		time.Sleep(20 * time.Millisecond)
		close(stop)
		wg.Wait()
		select {
		case msg := <-bad:
			t.Fatal(msg)
		default:
		}
		assert.Equal(t, newContent, mustRead(t, b, key))
	})
}

func TestDeleteDuringRead(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	// A short range reads each piece with a statement of its own. A change
	// that commits part way makes the next piece fail with
	// FailedPrecondition; every byte before it is the version the reader was
	// opened on.
	for _, change := range []string{"deleted", "overwritten"} {
		t.Run("short read, "+change, func(t *testing.T) {
			b := newBucket(t, db)
			key := "short-" + change
			want := noise(6, 1<<20)
			require.NoError(t, b.WriteAll(ctx, key, want, nil))

			r, err := b.NewReader(ctx, key, nil)
			require.NoError(t, err)
			defer func() { _ = r.Close() }()
			prefix := make([]byte, 100)
			_, err = io.ReadFull(r, prefix)
			require.NoError(t, err)
			assert.Equal(t, want[:100], prefix)

			if change == "deleted" {
				require.NoError(t, b.Delete(ctx, key))
			} else {
				require.NoError(t, b.WriteAll(ctx, key, noise(7, 1<<20), nil))
			}

			rest, err := io.ReadAll(r)
			require.Error(t, err, "a short read went on after its object changed")
			assert.Equal(t, gcerrors.FailedPrecondition, gcerrors.Code(err))
			require.Less(t, len(rest), len(want)-100)
			assert.True(t, bytes.Equal(want[100:100+len(rest)], rest), "a short read mixed versions")
		})
	}

	// A long range reads inside one snapshot: a change that commits after its
	// first Read changes nothing it returns.
	for _, change := range []string{"deleted", "overwritten"} {
		t.Run("read in a snapshot, "+change, func(t *testing.T) {
			b := newSmallBucket(t, db)
			key := "snapshot-" + change
			want := pattern(60) // longer than the small bucket's 20-byte limit for a short read
			require.NoError(t, b.WriteAll(ctx, key, want, nil))

			r, err := b.NewReader(ctx, key, nil)
			require.NoError(t, err)
			prefix := make([]byte, 10)
			_, err = io.ReadFull(r, prefix)
			require.NoError(t, err)

			if change == "deleted" {
				require.NoError(t, b.Delete(ctx, key))
			} else {
				require.NoError(t, b.WriteAll(ctx, key, bytes.Repeat([]byte("x"), 61), nil))
			}

			rest, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			assert.Equal(t, want, append(prefix, rest...), "a read in a snapshot mixed versions or stopped early")
		})
	}
}

func TestSlowReadInSnapshot(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	// A pool whose server ends any transaction idle for over a second, as a
	// strict database setting would. A long range's snapshot waits between
	// pieces for as long as its caller takes; that must not end it.
	strict, err := sql.Open("pgx", dsnWith(t, "idle_in_transaction_session_timeout=1000"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = strict.Close() })
	b := newSmallBucket(t, strict)

	want := pattern(60)
	require.NoError(t, b.WriteAll(ctx, "slow", want, nil))
	r, err := b.NewReader(ctx, "slow", nil)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	head := make([]byte, 7)
	_, err = io.ReadFull(r, head)
	require.NoError(t, err)

	time.Sleep(2 * time.Second)

	rest, err := io.ReadAll(r)
	require.NoError(t, err, "a read in a snapshot was ended while its caller was slow")
	assert.Equal(t, want, append(head, rest...))
}

func TestChangeBeforeFirstRead(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	buckets(t, db, func(t *testing.T, b *blob.Bucket, _ bool) {
		for _, change := range []string{"deleted", "overwritten"} {
			t.Run(change, func(t *testing.T) {
				key := "opened-then-" + change
				require.NoError(t, b.WriteAll(ctx, key, pattern(60), nil))
				r, err := b.NewReader(ctx, key, nil)
				require.NoError(t, err)
				defer func() { _ = r.Close() }()

				if change == "deleted" {
					require.NoError(t, b.Delete(ctx, key))
				} else {
					require.NoError(t, b.WriteAll(ctx, key, bytes.Repeat([]byte("x"), 61), nil))
				}

				got, err := io.ReadAll(r)
				require.Error(t, err)
				assert.Equal(t, gcerrors.FailedPrecondition, gcerrors.Code(err))
				assert.Empty(t, got, "a read of a changed object returned bytes")
			})
		}
	})
}

func TestIfNotExist(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newSmallBucket(t, db)
	ctx := context.Background()

	first := []byte("the first content, in three chunks")
	opts := &blob.WriterOptions{ContentType: "text/plain", IfNotExist: true}
	require.NoError(t, b.WriteAll(ctx, "conditional", first, opts))
	rowsBefore, _, _ := chunkRows(t, db, "conditional")

	err := b.WriteAll(ctx, "conditional", []byte("the second content, which loses"), opts)
	require.Error(t, err)
	assert.Equal(t, gcerrors.FailedPrecondition, gcerrors.Code(err))
	assert.Equal(t, first, mustRead(t, b, "conditional"))

	rows, _, contents := chunkRows(t, db, "conditional")
	assert.Equal(t, rowsBefore, rows, "the refused write left chunks behind")
	assert.Equal(t, 1, contents)
}

func TestCopy(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newSmallBucket(t, db)
	ctx := context.Background()

	src := pattern(30)
	require.NoError(t, b.WriteAll(ctx, "copy/src", src, &blob.WriterOptions{ContentType: "image/png"}))
	require.NoError(t, b.Copy(ctx, "copy/dst", "copy/src", nil))
	assert.Equal(t, src, mustRead(t, b, "copy/dst"))
	attrs, err := b.Attributes(ctx, "copy/dst")
	require.NoError(t, err)
	assert.Equal(t, "image/png", attrs.ContentType)

	// The copy has chunks of its own: deleting or replacing the source leaves
	// it as it was.
	require.NoError(t, b.Delete(ctx, "copy/src"))
	assert.Equal(t, src, mustRead(t, b, "copy/dst"))

	other := []byte("other bytes")
	require.NoError(t, b.WriteAll(ctx, "copy/other", other, nil))
	require.NoError(t, b.Copy(ctx, "copy/dst", "copy/other", nil))
	assert.Equal(t, other, mustRead(t, b, "copy/dst"))
	rows, size, contents := chunkRows(t, db, "copy/dst")
	assert.Equal(t, 2, rows, "the copy left the content it replaced behind")
	assert.EqualValues(t, len(other), size)
	assert.Equal(t, 1, contents)

	require.NoError(t, b.Copy(ctx, "copy/dst", "copy/dst", nil))
	assert.Equal(t, other, mustRead(t, b, "copy/dst"))
	rows, _, _ = chunkRows(t, db, "copy/dst")
	assert.Equal(t, 2, rows, "a copy onto itself doubled the chunks")

	err = b.Copy(ctx, "copy/new", "copy/absent", nil)
	require.Error(t, err)
	assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
}

func TestAttributesKept(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	buckets(t, db, func(t *testing.T, b *blob.Bucket, _ bool) {
		content := pattern(50)
		sum := md5.Sum(content)
		metadata := map[string]string{"owner": "someone", "nul\x00key": "a\x00value"}
		opts := &blob.WriterOptions{
			ContentType:        "image/png",
			CacheControl:       "private, max-age=60",
			ContentDisposition: `attachment; filename="photo.png"`,
			ContentEncoding:    "identity",
			ContentLanguage:    "en",
			Metadata:           metadata,
			ContentMD5:         sum[:],
		}
		require.NoError(t, b.WriteAll(ctx, "kept", content, opts))

		attrs, err := b.Attributes(ctx, "kept")
		require.NoError(t, err)
		assert.Equal(t, "image/png", attrs.ContentType)
		assert.Equal(t, "private, max-age=60", attrs.CacheControl)
		assert.Equal(t, `attachment; filename="photo.png"`, attrs.ContentDisposition)
		assert.Equal(t, "identity", attrs.ContentEncoding)
		assert.Equal(t, "en", attrs.ContentLanguage)
		assert.Equal(t, metadata, attrs.Metadata)
		assert.Equal(t, sum[:], attrs.MD5)
		assert.Regexp(t, `^"[0-9a-f-]{36}"$`, attrs.ETag)
		assert.False(t, attrs.CreateTime.After(attrs.ModTime))

		// An overwrite is a new object: its own ETag, and only what its own
		// write set.
		plain := []byte("plain")
		require.NoError(t, b.WriteAll(ctx, "kept", plain, nil))
		again, err := b.Attributes(ctx, "kept")
		require.NoError(t, err)
		assert.NotEqual(t, attrs.ETag, again.ETag)
		assert.Empty(t, again.CacheControl)
		assert.Empty(t, again.ContentDisposition)
		assert.Empty(t, again.ContentEncoding)
		assert.Empty(t, again.ContentLanguage)
		assert.Nil(t, again.Metadata)
		plainSum := md5.Sum(plain)
		assert.Equal(t, plainSum[:], again.MD5)

		// A copy keeps every attribute, under an ETag of its own.
		require.NoError(t, b.WriteAll(ctx, "kept", content, opts))
		attrs, err = b.Attributes(ctx, "kept")
		require.NoError(t, err)
		require.NoError(t, b.Copy(ctx, "kept-copy", "kept", nil))
		copied, err := b.Attributes(ctx, "kept-copy")
		require.NoError(t, err)
		assert.NotEqual(t, attrs.ETag, copied.ETag)
		kept := func(a *blob.Attributes) []any {
			return []any{a.ContentType, a.CacheControl, a.ContentDisposition, a.ContentEncoding, a.ContentLanguage,
				a.Metadata, a.MD5, a.Size}
		}
		assert.Equal(t, kept(attrs), kept(copied))
		assert.Equal(t, content, mustRead(t, b, "kept-copy"))
	})
}

func TestKeyWithNULRefused(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	// The SDK refuses a key that is not UTF-8. U+0000 is UTF-8, but a
	// PostgreSQL text value cannot hold it, so the driver refuses it with the
	// same code instead of passing on the database's error.
	const key = "nul\x00key"
	require.NoError(t, b.WriteAll(ctx, "plain", []byte("x"), nil))
	invalid := map[string]error{
		"empty key": b.WriteAll(ctx, "", []byte("x"), nil),
		"write":     b.WriteAll(ctx, key, []byte("x"), nil),
		"copy to":   b.Copy(ctx, key, "plain", nil),
		"copy from": b.Copy(ctx, "plain-copy", key, nil),
		"delete":    b.Delete(ctx, key),
		"header":    b.WriteAll(ctx, "header", []byte("x"), &blob.WriterOptions{CacheControl: "no\x00cache"}),
		// The SDK checks neither header for UTF-8; PostgreSQL text needs it.
		"header not UTF-8": b.WriteAll(ctx, "header-utf8", []byte("x"),
			&blob.WriterOptions{CacheControl: "max-age=\xff"}),
		"disposition not UTF-8": b.WriteAll(ctx, "header-utf8", []byte("x"),
			&blob.WriterOptions{ContentDisposition: "attachment; filename=\"\xc3\x28\""}),
		"attributes": func() error { _, err := b.Attributes(ctx, key); return err }(),
		"read":       func() error { _, err := b.NewReader(ctx, key, nil); return err }(),
		"page token": func() error {
			_, _, err := b.ListPage(ctx, []byte("p\xff"), 10, &blob.ListOptions{Prefix: "p"})
			return err
		}(),
	}
	for name, err := range invalid {
		require.Error(t, err, name)
		assert.Equal(t, gcerrors.InvalidArgument, gcerrors.Code(err), name)
	}
	for _, refused := range []string{"header", "header-utf8"} {
		_, err := b.Attributes(ctx, refused)
		assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err), "a refused write left %s behind", refused)
	}

	objects, next, err := b.ListPage(ctx, blob.FirstPageToken, 10, &blob.ListOptions{Prefix: "nul\x00"})
	require.NoError(t, err)
	assert.Empty(t, objects)
	assert.Empty(t, next)
}

func TestSignedURLUnimplemented(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)

	_, err := b.SignedURL(context.Background(), "anything", nil)
	require.Error(t, err)
	assert.Equal(t, gcerrors.Unimplemented, gcerrors.Code(err))
}

func TestURLScheme(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	require.NoError(t, RegisterPool(ctx, db))
	for _, table := range tables {
		assert.True(t, tableExists(t, db, table), "RegisterPool should create %s at startup", table)
	}

	b, err := blob.OpenBucket(ctx, Scheme+"://")
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	want := []byte("through the URL scheme")
	require.NoError(t, b.WriteAll(ctx, "url", want, nil))
	assert.Equal(t, want, mustRead(t, b, "url"))

	_, err = blob.OpenBucket(ctx, Scheme+"://with-a-host")
	require.Error(t, err)
}

// TestHTTPRangeReadsLittle serves an object the way Homebox serves an
// attachment, through http.ServeContent, and counts the bytes PostgreSQL
// sends for each request. After a Seek the SDK reopens its reader from the
// new offset to the end of the object, so the driver is never told how short
// an HTTP range is; it must still read about what the response sends.
func TestHTTPRangeReadsLittle(t *testing.T) {
	db, received := countingDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const key, size = "group/documents/photo", 10 << 20
	want := noise(9, size)
	require.NoError(t, b.WriteAll(ctx, key, want, &blob.WriterOptions{ContentType: "application/octet-stream"}))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, err := b.NewReader(r.Context(), key, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = file.Close() }()
		http.ServeContent(w, r, r.URL.Query().Get("name"), time.Time{}, file)
	}))
	t.Cleanup(server.Close)

	// A generous bound on one 64 KiB piece, the object's row and the protocol.
	const piece = 256 << 10
	tests := []struct {
		name, file, ranges string
		status             int
		body               []byte
		most               int64
	}{
		{"first two bytes", "photo.jpg", "bytes=0-1", http.StatusPartialContent, want[:2], piece},
		{"two bytes in the middle", "photo.jpg", "bytes=5000000-5000001", http.StatusPartialContent,
			want[5000000:5000002], piece},
		{"last two bytes", "photo.jpg", "bytes=-2", http.StatusPartialContent, want[size-2:], piece},
		// For a name whose extension Go does not know, as for a .heic or .mov
		// attachment, ServeContent first reads 512 bytes to sniff the type and
		// seeks back, so the SDK opens a second reader.
		{"two bytes, type sniffed", "photo", "bytes=0-1", http.StatusPartialContent, want[:2], 2 * piece},
		{"the whole object, type sniffed", "photo", "", http.StatusOK, want, size + 2*piece},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/?name="+tt.file, nil)
			require.NoError(t, err)
			if tt.ranges != "" {
				req.Header.Set("Range", tt.ranges)
			}
			received.Store(0)
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			got := received.Load()
			t.Logf("PostgreSQL sent %d bytes for a response of %d", got, len(body))

			assert.Equal(t, tt.status, resp.StatusCode)
			assert.True(t, bytes.Equal(tt.body, body), "the response has %d bytes, not the %d asked for", len(body), len(tt.body))
			assert.LessOrEqual(t, got, tt.most, "PostgreSQL sent %d bytes for a response of %d", got, len(body))
		})
	}
}

func TestReadAfterClose(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	// The SDK passes a Read after Close on to the driver. Every reader, the
	// small bucket's long one with its snapshot included, refuses it.
	buckets(t, db, func(t *testing.T, b *blob.Bucket, _ bool) {
		require.NoError(t, b.WriteAll(ctx, "closed", pattern(60), nil))
		r, err := b.NewReader(ctx, "closed", nil)
		require.NoError(t, err)
		head := make([]byte, 10)
		_, err = io.ReadFull(r, head)
		require.NoError(t, err)
		require.NoError(t, r.Close())

		n, err := r.Read(make([]byte, 10))
		assert.Zero(t, n)
		require.Error(t, err)
		assert.Equal(t, gcerrors.FailedPrecondition, gcerrors.Code(err))
	})
}

func TestBeforeReadOnlyForAnObject(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	// The driver contract: BeforeRead is called once before any data is read,
	// and not at all when NewRangeReader fails first.
	calls := 0
	opts := &blob.ReaderOptions{BeforeRead: func(func(any) bool) error { calls++; return nil }}
	_, err := b.NewReader(ctx, "absent", opts)
	require.Error(t, err)
	assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
	assert.Zero(t, calls, "BeforeRead was called for a key that is absent")

	require.NoError(t, b.WriteAll(ctx, "present", []byte("present"), nil))
	r, err := b.NewReader(ctx, "present", opts)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, []byte("present"), got)
	assert.Equal(t, 1, calls)
}

func TestListPagesWhileKeysChange(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	write := func(keys ...string) {
		for _, key := range keys {
			require.NoError(t, b.WriteAll(ctx, key, []byte(key), nil))
		}
	}
	page := func(prefix, delimiter string, token []byte) ([]string, []byte) {
		objects, next, err := b.ListPage(ctx, token, 2, &blob.ListOptions{Prefix: prefix, Delimiter: delimiter})
		require.NoError(t, err)
		keys := make([]string, 0, len(objects))
		for _, object := range objects {
			keys = append(keys, object.Key)
		}
		return keys, next
	}
	rest := func(prefix, delimiter string, token []byte) []string {
		var keys []string
		for len(token) > 0 {
			got, next := page(prefix, delimiter, token)
			keys = append(keys, got...)
			token = next
		}
		return keys
	}

	// The driver contract: every entry of a later page sorts after every
	// entry of the pages before it. Entries that were there throughout are
	// each listed once.
	for i, delimiter := range []string{"/", ""} {
		name := "slash delimiter"
		if delimiter == "" {
			name = "no delimiter"
		}
		t.Run(name, func(t *testing.T) {
			entry := func(prefix, dir string) string {
				if delimiter == "" {
					return prefix + dir + "/1"
				}
				return prefix + dir + "/"
			}

			// A write between two pages, of keys that sort into the first.
			w := fmt.Sprintf("write%d/", i)
			write(w+"a/1", w+"b/1", w+"c/1", w+"d/1")
			first, token := page(w, delimiter, blob.FirstPageToken)
			require.Equal(t, []string{entry(w, "a"), entry(w, "b")}, first)
			write(w+"a0/1", w+"a1/1")
			assert.Equal(t, []string{entry(w, "c"), entry(w, "d")}, rest(w, delimiter, token))

			// A delete between two pages, of an entry of the first.
			d := fmt.Sprintf("delete%d/", i)
			write(d+"a/1", d+"b/1", d+"c/1", d+"d/1")
			first, token = page(d, delimiter, blob.FirstPageToken)
			require.Equal(t, []string{entry(d, "a"), entry(d, "b")}, first)
			require.NoError(t, b.Delete(ctx, d+"a/1"))
			assert.Equal(t, []string{entry(d, "c"), entry(d, "d")}, rest(d, delimiter, token))
		})
	}
}

func TestListByteOrder(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	// In UTF-8 byte order, which the driver contract asks for, B (0x42) comes
	// before a (0x61), and é (0xC3 0xA9) after f (0x66). A language's order,
	// such as ICU's en-US, puts o/a first and o/é before o/f.
	keys := []string{"o/é/1", "o/a", "o/f", "o/B", "o/ab", "o/é", "o/a-c"}
	for _, key := range keys {
		require.NoError(t, b.WriteAll(ctx, key, []byte(key), nil))
	}
	flat := slices.Sorted(slices.Values(keys))
	require.Equal(t, []string{"o/B", "o/a", "o/a-c", "o/ab", "o/f", "o/é", "o/é/1"}, flat)
	delimited := []string{"o/B", "o/a", "o/a-c", "o/ab", "o/f", "o/é", "o/é/"}

	list := func(b *blob.Bucket, delimiter string) []string {
		var got []string
		token := blob.FirstPageToken
		for {
			objects, next, err := b.ListPage(ctx, token, 2, &blob.ListOptions{Prefix: "o/", Delimiter: delimiter})
			require.NoError(t, err)
			for _, object := range objects {
				got = append(got, object.Key)
			}
			if len(next) == 0 {
				return got
			}
			token = next
		}
	}

	t.Run("the key columns are C", func(t *testing.T) {
		for _, table := range tables {
			var collation string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT c.collname FROM pg_attribute a
JOIN pg_collation c ON c.oid = a.attcollation
WHERE a.attrelid = to_regclass($1) AND a.attname = 'key'`, table).Scan(&collation))
			assert.Equal(t, "C", collation, "the key column of %s", table)
		}
		assert.Equal(t, flat, list(b, ""))
		assert.Equal(t, delimited, list(b, "/"))
	})

	t.Run("a key column in a language's order", func(t *testing.T) {
		// postgres:17-alpine's default collation compares like C under musl,
		// so ICU's en-US stands in for a database whose default does not. The
		// queries order by COLLATE "C" themselves.
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE `+ObjectsTable+` ALTER COLUMN "key" TYPE text COLLATE "en-US-x-icu"`); err != nil {
			t.Skipf("this PostgreSQL has no ICU collation: %v", err)
		}
		var first string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT "key" FROM `+ObjectsTable+
			` WHERE starts_with("key", 'o/') ORDER BY "key" LIMIT 1`).Scan(&first))
		require.Equal(t, "o/a", first, "en-US-x-icu orders the column like a language")

		// A new pool: the old one's prepared statements describe the column
		// as it was.
		fresh, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = fresh.Close() })
		b := newBucket(t, fresh)
		assert.Equal(t, flat, list(b, ""))
		assert.Equal(t, delimited, list(b, "/"))
	})
}

// waitForLockWaiter returns once a statement on the test database waits for
// a lock.
func waitForLockWaiter(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, db.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no statement began to wait for the row lock")
}

func TestWritesWhenTheDefaultIsRepeatableRead(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	// A database or role may set default_transaction_isolation to repeatable
	// read. The writer, Delete and Copy ask for READ COMMITTED themselves, so
	// a change that another transaction commits while they wait is no
	// serialization failure.
	rr, err := sql.Open("pgx", dsnWith(t, "default_transaction_isolation=repeatable%20read"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rr.Close() })
	var isolation string
	require.NoError(t, rr.QueryRowContext(ctx, `SHOW default_transaction_isolation`).Scan(&isolation))
	require.Equal(t, "repeatable read", isolation)
	b := newSmallBucket(t, rr)

	// holdRow locks the key's row in another transaction, starts op, waits
	// until op waits for the lock, and commits a change to the row.
	holdRow := func(key string, op func() error) error {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `UPDATE `+ObjectsTable+` SET mod_time = mod_time WHERE "key" = $1`, key)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- op() }()
		waitForLockWaiter(t, db)
		require.NoError(t, tx.Commit())
		return <-done
	}

	t.Run("a write that commits after another write to its key", func(t *testing.T) {
		const key = "rr/write"
		w, err := b.NewWriter(ctx, key, &blob.WriterOptions{ContentType: "text/plain"})
		require.NoError(t, err)
		mine := bytes.Repeat([]byte("1"), 10)
		_, err = w.Write(mine) // one chunk sent: the writer's transaction has its snapshot
		require.NoError(t, err)
		require.NoError(t, b.WriteAll(ctx, key, bytes.Repeat([]byte("2"), 12), nil))
		require.NoError(t, w.Close(), "the write that committed last failed")

		assert.Equal(t, mine, mustRead(t, b, key))
		rows, size, contents := chunkRows(t, db, key)
		assert.Equal(t, 2, rows, "the replaced write's chunks were left behind")
		assert.EqualValues(t, len(mine), size)
		assert.Equal(t, 1, contents)
	})

	t.Run("a delete while another transaction changes the row", func(t *testing.T) {
		const key = "rr/delete"
		require.NoError(t, b.WriteAll(ctx, key, pattern(20), nil))
		require.NoError(t, holdRow(key, func() error { return b.Delete(ctx, key) }))
		_, err := b.Attributes(ctx, key)
		assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
		rows, _, _ := chunkRows(t, db, key)
		assert.Zero(t, rows)
	})

	t.Run("a copy onto a row another transaction changes", func(t *testing.T) {
		const src, dst = "rr/src", "rr/dst"
		want := pattern(30)
		require.NoError(t, b.WriteAll(ctx, src, want, nil))
		require.NoError(t, b.WriteAll(ctx, dst, []byte("old"), nil))
		require.NoError(t, holdRow(dst, func() error { return b.Copy(ctx, dst, src, nil) }))
		assert.Equal(t, want, mustRead(t, b, dst))
		rows, _, contents := chunkRows(t, db, dst)
		assert.Equal(t, 5, rows)
		assert.Equal(t, 1, contents)
	})
}
