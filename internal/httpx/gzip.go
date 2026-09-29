package httpx

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// CopyBody streams a large object to the client, gzip-compressed at BestSpeed
// when the request accepts it. A snapshot is mostly zero pages: a move sent a
// 1 GiB memfile and a 1 GiB overlay that compress to ~280 MB, over a link that
// moves ~60 MB/s, so the transfer dominated the move. Go's client asks for
// gzip and decompresses on its own, so callers need no change. A Range read is
// never compressed; size is sent as Content-Length when it is not.
// ponytail: recompresses on every read; store objects compressed if the
// control plane's CPU becomes the bottleneck.
func CopyBody(w http.ResponseWriter, r *http.Request, size int64, src io.Reader) error {
	if r.Header.Get("Range") != "" || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		if size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.WriteHeader(http.StatusOK)
		_, err := io.Copy(w, src)
		return err
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.WriteHeader(http.StatusOK)
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed) //nolint:errcheck // BestSpeed is a valid level
	if _, err := io.Copy(zw, src); err != nil {
		return err
	}
	return zw.Close()
}
