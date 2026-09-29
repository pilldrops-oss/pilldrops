package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The HTTP protocol for artifacts. Handler serves any StorageProvider and
// ControlPlane is the client for it. The wire format is S3-shaped (keys in
// the path, Range header, 404/206) so an object store can replace it later.

const (
	// ArtifactPathPrefix is where a Handler expects to be mounted.
	ArtifactPathPrefix = "/v1/artifacts/"
	// ListPath answers a prefix listing.
	ListPath = "/v1/artifacts"
)

// Handler serves a StorageProvider over HTTP with no authentication of its own.
// A mount behind the shared node key must be ReadOnly, or any node could
// replace the rootfs every other node boots.
type Handler struct {
	store StorageProvider
	// MaxUploadBytes bounds a PUT. Zero means unlimited.
	MaxUploadBytes int64
	// ReadOnly refuses PUT and DELETE with 405.
	ReadOnly bool
	// Direct, when the store can presign, answers a GET with a redirect to a
	// URL valid this long, so the bytes go from the bucket to the reader
	// instead of through this process. Zero proxies them.
	Direct time.Duration
}

func NewHandler(store StorageProvider) *Handler { return &Handler{store: store} }

// NewReadOnlyHandler serves reads only; this is the mount for nodes.
func NewReadOnlyHandler(store StorageProvider) *Handler {
	return &Handler{store: store, ReadOnly: true}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == ListPath {
		h.list(w, r)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, ArtifactPathPrefix)
	if key == r.URL.Path || key == "" {
		httpErr(w, http.StatusBadRequest, "artifact key is required")
		return
	}
	if err := ValidateKey(key); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.get(w, r, key)
	case http.MethodHead:
		h.head(w, r, key)
	case http.MethodPut, http.MethodDelete:
		if h.ReadOnly {
			httpErr(w, http.StatusMethodNotAllowed,
				"this artifact store is read-only here; publish through POST /v1/templates/{id}/artifacts")
			return
		}
		if r.Method == http.MethodPut {
			h.put(w, r, key)
		} else {
			h.del(w, r, key)
		}
	default:
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, key string) {
	if p, ok := h.store.(Presigner); ok && h.Direct > 0 {
		// Go's client follows the redirect with the Range header kept and the
		// Authorization header dropped, since the bucket is another host.
		if u, err := p.Presign(http.MethodGet, key, h.Direct); err == nil {
			http.Redirect(w, r, u, http.StatusTemporaryRedirect)
			return
		}
	}
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		off, length, ok := parseRange(rangeHdr)
		if !ok {
			httpErr(w, http.StatusRequestedRangeNotSatisfiable, "malformed Range header")
			return
		}
		rc, err := h.store.GetRange(r.Context(), key, off, length)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.Copy(w, rc)
		return
	}

	rc, err := h.store.Get(r.Context(), key)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	defer rc.Close()
	size := int64(-1)
	if info, err := h.store.Stat(r.Context(), key); err == nil {
		size = info.Size
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	_ = httpx.CopyBody(w, r, size, rc)
}

func (h *Handler) head(w http.ResponseWriter, r *http.Request, key string) {
	info, err := h.store.Stat(r.Context(), key)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", info.Modified.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request, key string) {
	body := r.Body
	if h.MaxUploadBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, h.MaxUploadBytes)
	}
	if err := h.store.Put(r.Context(), key, body, r.ContentLength); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpErr(w, http.StatusRequestEntityTooLarge, "artifact exceeds the upload limit")
			return
		}
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) del(w http.ResponseWriter, r *http.Request, key string) {
	if err := h.store.Delete(r.Context(), key); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	keys, err := h.store.List(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// writeStoreErr maps a store error to a status; the client maps it back.
func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalidKey):
		httpErr(w, http.StatusBadRequest, err.Error())
	default:
		httpErr(w, http.StatusInternalServerError, err.Error())
	}
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseRange handles the single-range form `bytes=off-end`. Multi-range is
// refused rather than partially honoured.
func parseRange(h string) (off, length int64, ok bool) {
	const p = "bytes="
	if !strings.HasPrefix(h, p) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, p)
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	start, end, found := strings.Cut(spec, "-")
	if !found || start == "" {
		return 0, 0, false
	}
	off, err := strconv.ParseInt(start, 10, 64)
	if err != nil || off < 0 {
		return 0, 0, false
	}
	if end == "" {
		// Open-ended; the store clamps to what exists.
		return off, 1<<62 - 1, true
	}
	last, err := strconv.ParseInt(end, 10, 64)
	if err != nil || last < off {
		return 0, 0, false
	}
	return off, last - off + 1, true
}

func rangeHeader(off, length int64) string {
	return fmt.Sprintf("bytes=%d-%d", off, off+length-1)
}
