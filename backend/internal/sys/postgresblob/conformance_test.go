// Tofu (2026-11-06): runs gocloud.dev's own conformance suite for blob drivers
// (gocloud.dev/blob/drivertest, part of the gocloud.dev module the app already
// requires) against Tofu's PostgreSQL driver. Like the other database tests it
// runs only when TEST_DATABASE_URL is set.
package postgresblob

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"gocloud.dev/blob/driver"
	"gocloud.dev/blob/drivertest"
)

// conformanceHarness gives the suite buckets on the test database. Every
// bucket it makes shares the database's two tables, which is what the suite
// asks of "the same storage bucket".
type conformanceHarness struct {
	db         *sql.DB
	chunkSize  int
	shortLimit int64
	firstFetch int64
}

func (h *conformanceHarness) MakeDriver(context.Context) (driver.Bucket, error) {
	return &bucket{db: h.db, chunkSize: h.chunkSize, shortLimit: h.shortLimit, firstFetch: h.firstFetch}, nil
}

// MakeDriverForNonexistentBucket returns no bucket: a database has no bucket
// that could be missing, and the suite skips that test for such a driver.
func (*conformanceHarness) MakeDriverForNonexistentBucket(context.Context) (driver.Bucket, error) {
	return nil, nil
}

// HTTPClient is only used for signed URLs, which the driver does not make.
func (*conformanceHarness) HTTPClient() *http.Client { return nil }

func (*conformanceHarness) SupportsCopy() bool { return true }

func (*conformanceHarness) SupportsNonSlashDelimiters() bool { return true }

func (*conformanceHarness) Close() {}

// notConformant lists the suite's cases the driver refuses by design, and
// why. Every other case of the suite must pass.
var notConformant = map[string]string{
	"TestKeys/ascii-1": "the key contains U+0000, which a PostgreSQL text value cannot hold; " +
		"the driver refuses it with InvalidArgument (TestKeyWithNULRefused)",
	"TestListWeirdKeys": "its setup writes the same key with U+0000 as TestKeys/ascii-1",
}

func TestConformance(t *testing.T) {
	db := testDB(t)

	// With the real sizes every object of the suite is one chunk, read in
	// one or two pieces without a snapshot; with 7-byte chunks, a 20-byte
	// limit for a read without a snapshot and a 3-byte first piece, the same
	// cases run through chunked writes, pieces inside a chunk and reads inside
	// a snapshot.
	for _, size := range []struct {
		name       string
		chunkSize  int
		shortLimit int64
		firstFetch int64
	}{
		{name: "real sizes", chunkSize: ChunkSize, shortLimit: ShortReadLimit, firstFetch: firstFetch},
		{name: "small chunks", chunkSize: 7, shortLimit: 20, firstFetch: 3},
	} {
		t.Run(size.name, func(t *testing.T) {
			reset(t, db)
			prefix := t.Name() + "/"
			drivertest.RunConformanceTests(t, func(_ context.Context, t *testing.T) (drivertest.Harness, error) {
				if reason, ok := notConformant[strings.TrimPrefix(t.Name(), prefix)]; ok {
					t.Skip(reason)
				}
				return &conformanceHarness{db: db, chunkSize: size.chunkSize, shortLimit: size.shortLimit,
					firstFetch: size.firstFetch}, nil
			}, nil)
		})
	}
}
