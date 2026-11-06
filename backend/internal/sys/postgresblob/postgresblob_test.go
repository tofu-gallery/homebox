// Tofu (2026-11-05): tests for Tofu's PostgreSQL blob driver. The database
// tests run only when TEST_DATABASE_URL points at a throwaway PostgreSQL
// database; without it they skip, as the rest of this repository's
// database-backed tests do.
package postgresblob

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	_ "github.com/jackc/pgx/v5/stdlib"
)

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

// reset drops the driver's table so each test starts from a known state and
// can observe the table being created again.
func reset(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+TableName)
	require.NoError(t, err)
}

func tableExists(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, TableName).Scan(&exists)
	require.NoError(t, err)
	return exists
}

func newBucket(t *testing.T, db *sql.DB) *blob.Bucket {
	t.Helper()
	b := NewBucket(db)
	t.Cleanup(func() { _ = b.Close() })
	return b
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

func TestSchemeRegistered(t *testing.T) {
	RegisterScheme()
	assert.True(t, blob.DefaultURLMux().ValidBucketScheme(Scheme))
}

func TestTableCreatedOnFirstUse(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	require.False(t, tableExists(t, db), "table exists before the driver is used")

	b := newBucket(t, db)
	require.False(t, tableExists(t, db), "NewBucket should not touch the database")

	require.NoError(t, b.WriteAll(context.Background(), "first", []byte("x"), nil))
	assert.True(t, tableExists(t, db), "table was not created by the first write")
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
	})

	t.Run("large", func(t *testing.T) {
		want := pattern(5<<20 + 17) // over the 2 KiB TOAST threshold and a few MiB
		require.NoError(t, b.WriteAll(ctx, "large", want, &blob.WriterOptions{ContentType: "application/octet-stream"}))
		assert.Equal(t, want, mustRead(t, b, "large"))

		var stored int64
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT octet_length(content) FROM `+TableName+` WHERE "key" = $1`, "large").Scan(&stored))
		assert.EqualValues(t, len(want), stored)
	})

	t.Run("survives a new pool", func(t *testing.T) {
		want := pattern(3 << 20)
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

func TestRangeRead(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const content = "0123456789"
	require.NoError(t, b.WriteAll(ctx, "range", []byte(content), &blob.WriterOptions{ContentType: "text/plain"}))

	tests := []struct {
		name   string
		offset int64
		length int64
		want   string
	}{
		{name: "start", offset: 0, length: 4, want: "0123"},
		{name: "middle", offset: 3, length: 4, want: "3456"},
		{name: "to end", offset: 8, length: -1, want: "89"},
		{name: "whole", offset: 0, length: -1, want: content},
		{name: "at end", offset: 10, length: -1, want: ""},
		{name: "past end", offset: 20, length: 5, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := b.NewRangeReader(ctx, "range", tt.offset, tt.length, nil)
			require.NoError(t, err)
			defer func() { _ = r.Close() }()

			got := new(bytes.Buffer)
			_, err = got.ReadFrom(r)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.String())

			attrs, err := b.Attributes(ctx, "range")
			require.NoError(t, err)
			assert.EqualValues(t, len(content), attrs.Size)
		})
	}

	t.Run("missing key", func(t *testing.T) {
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
	b := newBucket(t, db)
	ctx := context.Background()

	require.NoError(t, b.WriteAll(ctx, "delete-me", []byte("gone"), nil))
	require.NoError(t, b.Delete(ctx, "delete-me"))

	_, err := b.ReadAll(ctx, "delete-me")
	require.Error(t, err)
	assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))

	err = b.Delete(ctx, "delete-me")
	require.Error(t, err)
	assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
}

func TestConcurrentWriters(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const keyA, keyB = "concurrent/a", "concurrent/b"
	wantA, wantB := pattern(1<<20), pattern(1<<20+9)
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

func TestReaderDuringWrite(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const key = "in-flight"
	oldContent := bytes.Repeat([]byte("o"), 4096)
	newContent := bytes.Repeat([]byte("n"), 8192)
	require.NoError(t, b.WriteAll(ctx, key, oldContent, nil))

	w, err := b.NewWriter(ctx, key, &blob.WriterOptions{ContentType: "application/octet-stream"})
	require.NoError(t, err)

	// Writing does not become visible until Close: a reader sees the old
	// object whole, never a prefix of the new one.
	_, err = w.Write(newContent[:len(newContent)/2])
	require.NoError(t, err)
	assert.Equal(t, oldContent, mustRead(t, b, key))

	_, err = w.Write(newContent[len(newContent)/2:])
	require.NoError(t, err)
	assert.Equal(t, oldContent, mustRead(t, b, key))

	require.NoError(t, w.Close())
	assert.Equal(t, newContent, mustRead(t, b, key))
}

func TestReaderWhileWriterCloses(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const key = "racing"
	oldContent := pattern(1 << 20)
	newContent := pattern(1<<20 + 3)
	require.NoError(t, b.WriteAll(ctx, key, oldContent, nil))

	w, err := b.NewWriter(ctx, key, &blob.WriterOptions{ContentType: "application/octet-stream"})
	require.NoError(t, err)
	_, err = w.Write(newContent)
	require.NoError(t, err)

	// Read in a loop while the writer commits. Every read must be one of the
	// two complete objects; a torn read would be a different length or bytes.
	stop := make(chan struct{})
	bad := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := b.ReadAll(ctx, key)
			if err != nil {
				bad <- fmt.Sprintf("read error: %v", err)
				return
			}
			if !bytes.Equal(got, oldContent) && !bytes.Equal(got, newContent) {
				bad <- fmt.Sprintf("torn read: got %d bytes", len(got))
				return
			}
		}
	}()

	require.NoError(t, w.Close())
	close(stop)
	wg.Wait()
	select {
	case msg := <-bad:
		t.Fatal(msg)
	default:
	}
	assert.Equal(t, newContent, mustRead(t, b, key))
}

func TestDeleteDuringRead(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	const key = "delete-during-read"
	want := pattern(1 << 20)
	require.NoError(t, b.WriteAll(ctx, key, want, nil))

	r, err := b.NewReader(ctx, key, nil)
	require.NoError(t, err)
	prefix := make([]byte, 100)
	_, err = r.Read(prefix)
	require.NoError(t, err)
	assert.Equal(t, want[:100], prefix)

	require.NoError(t, b.Delete(ctx, key))

	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, want[100:], rest)
	require.NoError(t, r.Close())

	_, err = b.ReadAll(ctx, key)
	require.Error(t, err)
	assert.Equal(t, gcerrors.NotFound, gcerrors.Code(err))
}

func TestIfNotExist(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	opts := &blob.WriterOptions{ContentType: "text/plain", IfNotExist: true}
	require.NoError(t, b.WriteAll(ctx, "conditional", []byte("first"), opts))

	err := b.WriteAll(ctx, "conditional", []byte("second"), opts)
	require.Error(t, err)
	assert.Equal(t, gcerrors.FailedPrecondition, gcerrors.Code(err))
	assert.Equal(t, []byte("first"), mustRead(t, b, "conditional"))
}

func TestCopy(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	b := newBucket(t, db)
	ctx := context.Background()

	want := pattern(64 << 10)
	require.NoError(t, b.WriteAll(ctx, "copy/src", want, nil))
	require.NoError(t, b.Copy(ctx, "copy/dst", "copy/src", nil))
	assert.Equal(t, want, mustRead(t, b, "copy/dst"))

	require.NoError(t, b.Delete(ctx, "copy/src"))
	assert.Equal(t, want, mustRead(t, b, "copy/dst"))
}

func TestURLScheme(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	require.NoError(t, RegisterPool(ctx, db))
	assert.True(t, tableExists(t, db), "RegisterPool should create the table at startup")

	b, err := blob.OpenBucket(ctx, Scheme+"://")
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	want := []byte("through the URL scheme")
	require.NoError(t, b.WriteAll(ctx, "url", want, nil))
	assert.Equal(t, want, mustRead(t, b, "url"))

	_, err = blob.OpenBucket(ctx, Scheme+"://with-a-host")
	require.Error(t, err)
}
