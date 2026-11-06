// Tofu (2026-11-06): this file is Tofu's addition; it is not part of upstream Homebox.
package v1

import (
	"net/http"
	"strings"
)

// tofuIgnoreMultipleRanges drops a Range header that names more than one
// range, so that http.ServeContent answers the attachment download with the
// whole file and 200 instead of a multipart/byteranges response. RFC 9110
// (section 14.2) lets a server ignore Range.
//
// ServeContent seeks once for each range it serves, and after a seek the blob
// SDK opens its reader again, from the range's start to the end of the
// object. With storage in a bucket or in PostgreSQL, every opening is a
// request or a query, and Go serves up to 200 ranges a request, so a few
// hundred bytes of request made the storage open and read an attachment up
// to 200 times. A single range, which is what browsers, video players and
// PDF viewers send, is served as before.
func tofuIgnoreMultipleRanges(r *http.Request) {
	if strings.Contains(r.Header.Get("Range"), ",") {
		r.Header.Del("Range")
	}
}
