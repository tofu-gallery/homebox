// Tofu (2026-11-06): tests for tofuIgnoreMultipleRanges, Tofu's addition.
package v1

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seekCounter is an attachment that counts the seeks ServeContent makes to a
// place other than the start. After each one the blob SDK opens its reader
// again.
type seekCounter struct {
	*bytes.Reader
	reopens int
}

func (s *seekCounter) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart && offset > 0 {
		s.reopens++
	}
	return s.Reader.Seek(offset, whence)
}

// oneByteRanges is n ranges of one byte each, a byte apart.
func oneByteRanges(n int) string {
	ranges := make([]string, n)
	for i := range ranges {
		ranges[i] = strconv.Itoa(2*i) + "-" + strconv.Itoa(2*i)
	}
	return "bytes=" + strings.Join(ranges, ",")
}

func TestTofuIgnoreMultipleRanges(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 100)
	tests := []struct {
		name    string
		ranges  string
		status  int
		body    []byte
		reopens int
	}{
		{name: "no range", status: http.StatusOK, body: content},
		{name: "one range", ranges: "bytes=10-19", status: http.StatusPartialContent, body: content[10:20], reopens: 1},
		{name: "a suffix range", ranges: "bytes=-5", status: http.StatusPartialContent, body: content[995:], reopens: 1},
		{name: "two ranges", ranges: "bytes=0-1,10-11", status: http.StatusOK, body: content},
		{name: "one range and a trailing comma", ranges: "bytes=10-19,", status: http.StatusOK, body: content},
		{name: "two hundred ranges", ranges: oneByteRanges(200), status: http.StatusOK, body: content},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := &seekCounter{Reader: bytes.NewReader(content)}
			req := httptest.NewRequest(http.MethodGet, "/attachment", nil)
			if tt.ranges != "" {
				req.Header.Set("Range", tt.ranges)
			}
			rec := httptest.NewRecorder()

			tofuIgnoreMultipleRanges(req)
			http.ServeContent(rec, req, "manual.pdf", time.Time{}, file)

			require.Equal(t, tt.status, rec.Code)
			assert.Equal(t, tt.body, rec.Body.Bytes())
			assert.Equal(t, tt.reopens, file.reopens, "how often ServeContent sought into the attachment")
		})
	}
}
