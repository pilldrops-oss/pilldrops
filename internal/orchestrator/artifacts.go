package orchestrator

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The template-level artifact API: publish (operator only) and describe. It
// lives on the control plane because publishing writes template_builds and
// artifacts, which the node database role cannot, and because the ready mark
// decides what every node boots; a node that could set it would turn one
// compromise into fleet-wide code execution.

const (
	// artifactBlockSize is the block map granularity. A constant, not a request
	// parameter, so two builds of one template stay diffable.
	artifactBlockSize = 2 << 20

	// defaultMaxArtifactBytes bounds a publish body so a hostile upload cannot fill the disk.
	defaultMaxArtifactBytes = 8 << 30

	// maxHeaderBytes bounds reading a block map back; the node cache uses the same ceiling.
	maxHeaderBytes = 64 << 20
)

// SetArtifactStore wires the fleet's shared artifact store. Absent, the
// artifact routes refuse rather than fall back to a local directory a node cannot read.
func (s *Server) SetArtifactStore(p artifact.StorageProvider) { s.artifacts.Store(&p) }

func (s *Server) registerArtifactRoutes(mux *http.ServeMux) {
	// More specific than the "/v1/" catch-all, so a publish is never proxied to a node.
	mux.HandleFunc("POST /v1/templates/{id}/artifacts", s.auth(s.handleTemplateArtifactPublish))
	mux.HandleFunc("GET /v1/templates/{id}/artifacts", s.auth(s.handleTemplateArtifactDescribe))
	s.registerArtifactReadRoutes(mux)
}

// artifactIndex returns the shared store, or nil after answering 503. It opens
// in the background at boot, so nil early in a process is a wait, not a fault.
func (s *Server) artifactIndex(w http.ResponseWriter) *store.Store {
	st := s.st.Load()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"the control plane's database connection is not up yet, so no build can be recorded")
		return nil
	}
	return st
}

// artifactStore returns the configured store, or nil after answering 503.
func (s *Server) artifactStore(w http.ResponseWriter) artifact.StorageProvider {
	if p := s.artifacts.Load(); p != nil && *p != nil {
		return *p
	}
	writeErr(w, http.StatusServiceUnavailable,
		"no artifact store is configured on this control plane: set ORCHESTRATOR_ARTIFACT_DIR "+
			"(refusing rather than writing artifacts somewhere a node cannot read them)")
	return nil
}

// isTemplateArtifactPath reports whether p is the publish/describe route.
// Shape-matched because capBody runs before routing.
func isTemplateArtifactPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/v1/templates/")
	if !ok {
		return false
	}
	id, tail, found := strings.Cut(rest, "/")
	return found && id != "" && tail == "artifacts"
}

// handleTemplateArtifactPublish publishes a build. The rootfs is the raw
// request body, streamed and hashed; everything else is a query parameter.
func (s *Server) handleTemplateArtifactPublish(w http.ResponseWriter, r *http.Request) {
	if !isOperator(r) {
		writeErr(w, http.StatusForbidden,
			"publishing a template artifact requires the operator key: it decides what every node in the fleet boots")
		return
	}
	st := s.artifactIndex(w)
	if st == nil {
		return
	}
	blobs := s.artifactStore(w)
	if blobs == nil {
		return
	}

	q := r.URL.Query()
	buildID := q.Get("build_id")
	if buildID == "" {
		buildID = newBuildID()
	}

	out, err := publishBuild(r.Context(), st, blobs, publishSpec{
		TemplateID:         r.PathValue("id"),
		BuildID:            buildID,
		OrgID:              q.Get("org"),
		Payload:            artifact.NameRootfs,
		KernelVersion:      q.Get("kernel_version"),
		FirecrackerVersion: q.Get("firecracker_version"),
		EnvdVersion:        q.Get("envd_version"),
		Body:               http.MaxBytesReader(w, r.Body, s.maxArtifactBytes()),
		Size:               r.ContentLength,
		Now:                s.now().Unix(),
	})
	if err != nil {
		writeErr(w, publishStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"build": out.Build, "artifacts": out.Objects})
}

// handleTemplateArtifactDescribe lists a template's builds for operators and
// the CLI. Nodes resolve builds from template_builds directly, not through this.
func (s *Server) handleTemplateArtifactDescribe(w http.ResponseWriter, r *http.Request) {
	st := s.artifactIndex(w)
	if st == nil {
		return
	}
	templateID := r.PathValue("id")
	if err := validTemplateID(templateID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Operators see every org; customers see only their authenticated org.
	org := ""
	if !isOperator(r) {
		org = fastOrg(r)
	}
	limit := httpx.QueryInt(r, "limit", 0)
	builds, err := st.TemplateBuilds(r.Context(), templateID, org, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Newest ready build, from the same rows rather than a second query.
	readyID := ""
	var objects []store.ArtifactObject
	for _, b := range builds {
		if b.Ready() {
			readyID = b.BuildID
			break
		}
	}
	if readyID != "" {
		if objects, err = st.ArtifactObjects(r.Context(), readyID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if builds == nil {
		builds = []store.TemplateBuild{}
	}
	if objects == nil {
		objects = []store.ArtifactObject{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"template_id":    templateID,
		"ready_build_id": readyID,
		"builds":         builds,
		"artifacts":      objects,
	})
}

// artifactKeys are the three objects one publish writes, composed once so no step can disagree.
type artifactKeys struct{ data, header, metadata string }

func buildKeys(buildID, payload string) (artifactKeys, error) {
	var k artifactKeys
	var err error
	if k.data, err = artifact.Key(buildID, payload); err != nil {
		return k, err
	}
	if k.header, err = artifact.HeaderKey(buildID, payload); err != nil {
		return k, err
	}
	if k.metadata, err = artifact.Key(buildID, artifact.NameMetadata); err != nil {
		return k, err
	}
	return k, nil
}

// verifyPublished reads all three objects back and checks they describe each
// other. Returns the metadata as the store holds it.
func verifyPublished(ctx context.Context, blobs artifact.StorageProvider, keys artifactKeys,
	size int64, sum [sha256.Size]byte) (artifact.Metadata, error) {

	info, err := blobs.Stat(ctx, keys.data)
	if err != nil {
		return artifact.Metadata{}, fmt.Errorf("the payload is not readable back from the store: %w", err)
	}
	if info.Size != size {
		return artifact.Metadata{}, fmt.Errorf("the store holds %d bytes under %s but %d were written",
			info.Size, keys.data, size)
	}

	rawHeader, err := readObject(ctx, blobs, keys.header, maxHeaderBytes)
	if err != nil {
		return artifact.Metadata{}, err
	}
	hdr, err := header.Unmarshal(rawHeader)
	if err != nil {
		return artifact.Metadata{}, fmt.Errorf("the header written for this build does not parse: %w", err)
	}
	got, ok := hdr.SelfChecksum()
	if !ok || got != sum {
		return artifact.Metadata{}, fmt.Errorf("the stored header's checksum does not match the bytes written")
	}
	if hdr.TotalSize != uint64(size) {
		return artifact.Metadata{}, fmt.Errorf("the stored header describes %d bytes, not %d", hdr.TotalSize, size)
	}

	rawMeta, err := readObject(ctx, blobs, keys.metadata, artifact.MaxMetadataBytes)
	if err != nil {
		return artifact.Metadata{}, err
	}
	meta, err := artifact.ParseMetadata(rawMeta)
	if err != nil {
		return artifact.Metadata{}, err
	}
	if meta.RootfsSize != size || meta.RootfsSHA256 != hex.EncodeToString(sum[:]) {
		return artifact.Metadata{}, fmt.Errorf("metadata.json describes a different payload than the one published")
	}
	return meta, nil
}

func readObject(ctx context.Context, blobs artifact.StorageProvider, key string, max int64) ([]byte, error) {
	rc, err := blobs.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("%s is not readable back from the store: %w", key, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, max))
	if err != nil {
		return nil, fmt.Errorf("reading %s back: %w", key, err)
	}
	return raw, nil
}

func putBytes(ctx context.Context, blobs artifact.StorageProvider, key string, raw []byte) error {
	if err := blobs.Put(ctx, key, bytes.NewReader(raw), int64(len(raw))); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	return nil
}

// hashingReader digests bytes on their way into the store, so the hash is of what landed.
type hashingReader struct {
	src io.Reader
	sum interface {
		io.Writer
		Sum([]byte) []byte
	}
	n int64
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.src.Read(p)
	if n > 0 {
		h.n += int64(n)
		_, _ = h.sum.Write(p[:n])
	}
	return n, err
}

func (h *hashingReader) digest() [sha256.Size]byte {
	var out [sha256.Size]byte
	copy(out[:], h.sum.Sum(nil))
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// maxArtifactBytes is the publish body ceiling (MAX_ARTIFACT_BYTES).
func (s *Server) maxArtifactBytes() int64 {
	if s.maxArtifact > 0 {
		return s.maxArtifact
	}
	return defaultMaxArtifactBytes
}

// validTemplateID applies the object-key character rule to a template id.
func validTemplateID(id string) error {
	if id == "" {
		return errors.New("a template id is required")
	}
	if len(id) > 128 {
		return errors.New("template id is longer than 128 bytes")
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("template id %q contains %q; only letters, digits, -, _ and . are allowed", id, r)
		}
	}
	return nil
}

func newBuildID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "bld-" + hex.EncodeToString(b[:])
}

// registerArtifactReadRoutes mounts the shared store for nodes to read.
// Read-only: every node holds a node key, and a writable mount would let one
// compromised node replace what every other node boots.
func (s *Server) registerArtifactReadRoutes(mux *http.ServeMux) {
	serve := func(w http.ResponseWriter, r *http.Request) {
		if !s.authNodeKey(r) {
			writeErr(w, http.StatusUnauthorized,
				"a node credential is required to read the shared artifact store")
			return
		}
		// Resolved per request: the store opens in the background after registration.
		p := s.artifactStore(w)
		if p == nil {
			return
		}
		h := artifact.NewReadOnlyHandler(p)
		h.Direct = s.artifactDirect
		h.ServeHTTP(w, r)
	}
	mux.HandleFunc(artifact.ArtifactPathPrefix, serve)
	mux.HandleFunc(artifact.ListPath, serve)
}

// authNodeKey reports whether the request carries a registered node's key or
// the operator key. A fetch carries no node id, so every node is compared,
// in constant time and without short-circuiting.
func (s *Server) authNodeKey(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		return false
	}
	ok := false
	for _, n := range s.reg.Nodes() {
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.nodeKeyFor(n.ID, n.Zone))) == 1 {
			ok = true
		}
	}
	if s.apiKey != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.apiKey)) == 1 {
		ok = true
	}
	return ok
}
