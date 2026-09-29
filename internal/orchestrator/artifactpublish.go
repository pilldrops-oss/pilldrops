package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The publish core, shared by the operator upload and the image-build path so
// the step order cannot drift between them. The ready mark must be written
// last, after every object has been read back, or the database can say ready
// while the bytes are missing.

// publishSpec is one publish: which build of which template, and its data object.
type publishSpec struct {
	TemplateID string
	BuildID    string
	OrgID      string

	// Payload is the data object's artifact name (artifact.NameRootfs or
	// artifact.NameImage), recorded in metadata.json so a node knows the runtime before fetching.
	Payload string

	KernelVersion      string
	FirecrackerVersion string
	EnvdVersion        string

	// Body is the data object; Size is the length hint for Put, -1 when unknown.
	Body io.Reader
	Size int64

	// Now is the publish timestamp, supplied by the caller so tests can control it.
	Now int64

	// RowBegun says the 'building' row already exists and this publish owns it.
	// A snapshot save writes the row before it answers 202, so a client polling
	// the name immediately sees "building" rather than "no such snapshot".
	RowBegun bool
}

// publishOutcome is what a completed publish produced.
type publishOutcome struct {
	Build   *store.TemplateBuild
	Objects []store.ArtifactObject
}

// publishFault is a publish failure that carries its HTTP status.
type publishFault struct {
	status int
	err    error
}

func (e *publishFault) Error() string { return e.err.Error() }
func (e *publishFault) Unwrap() error { return e.err }

// publishStatus is the HTTP status for a publish failure, 500 when err carries none.
func publishStatus(err error) int {
	var f *publishFault
	if errors.As(err, &f) {
		return f.status
	}
	return http.StatusInternalServerError
}

func faultf(status int, format string, a ...any) *publishFault {
	return &publishFault{status: status, err: fmt.Errorf(format, a...)}
}

// publishBuild runs the publish steps; the caller reports failures. After step
// 1 any failure marks the 'building' row failed, best effort.
func publishBuild(ctx context.Context, st *store.Store, blobs artifact.StorageProvider,
	spec publishSpec) (publishOutcome, error) {

	var out publishOutcome
	if err := validTemplateID(spec.TemplateID); err != nil {
		return out, &publishFault{status: http.StatusBadRequest, err: err}
	}
	if err := artifact.ValidateBuildID(spec.BuildID); err != nil {
		return out, &publishFault{status: http.StatusBadRequest, err: err}
	}
	payload := spec.Payload
	if payload == "" {
		payload = artifact.NameRootfs
	}
	keys, err := buildKeys(spec.BuildID, payload)
	if err != nil {
		return out, &publishFault{status: http.StatusBadRequest, err: err}
	}
	org := spec.OrgID
	if org == "" {
		org = "default"
	}

	// Step 1: the 'building' row, before any byte is written.
	begin := store.TemplateBuild{
		BuildID: spec.BuildID, TemplateID: spec.TemplateID, OrgID: org, CreatedAt: spec.Now,
	}
	if !spec.RowBegun {
		if err := st.BeginTemplateBuild(ctx, begin); err != nil {
			if existing, _ := st.TemplateBuildByID(ctx, spec.BuildID); existing != nil {
				return out, faultf(http.StatusConflict,
					"build %q already exists (template %q, status %q) — publishing over it would replace "+
						"objects another build claims", spec.BuildID, existing.TemplateID, existing.Status)
			}
			return out, &publishFault{status: http.StatusInternalServerError, err: err}
		}
	}
	fail := func(err error) (publishOutcome, error) {
		_ = st.FailTemplateBuild(context.WithoutCancel(ctx), spec.BuildID)
		return out, err
	}

	// Step 2: the data object, hashed while streaming.
	hr := &hashingReader{src: spec.Body, sum: sha256.New()}
	if err := blobs.Put(ctx, keys.data, hr, spec.Size); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fail(faultf(http.StatusRequestEntityTooLarge, "artifact exceeds the publish limit"))
		}
		return fail(faultf(http.StatusBadGateway, "writing %s: %v", keys.data, err))
	}
	size, sum := hr.n, hr.digest()
	if size == 0 {
		return fail(faultf(http.StatusBadRequest,
			"the request body was empty: a published %s cannot be zero bytes", payload))
	}

	// Step 3: the block map header. A node refuses an artifact without one.
	hdr, err := header.New(spec.BuildID, artifactBlockSize, uint64(size), sum, spec.Now)
	if err != nil {
		return fail(&publishFault{status: http.StatusInternalServerError, err: err})
	}
	rawHeader, err := hdr.Marshal()
	if err != nil {
		return fail(&publishFault{status: http.StatusInternalServerError, err: err})
	}
	if err := putBytes(ctx, blobs, keys.header, rawHeader); err != nil {
		return fail(&publishFault{status: http.StatusBadGateway, err: err})
	}

	// Step 4: metadata.json.
	meta := artifact.Metadata{
		BuildID:            spec.BuildID,
		TemplateID:         spec.TemplateID,
		Payload:            payload,
		KernelVersion:      spec.KernelVersion,
		FirecrackerVersion: spec.FirecrackerVersion,
		EnvdVersion:        spec.EnvdVersion,
		RootfsSize:         size,
		RootfsSHA256:       hex.EncodeToString(sum[:]),
		CreatedUnix:        spec.Now,
	}
	rawMeta, err := meta.Marshal()
	if err != nil {
		return fail(&publishFault{status: http.StatusInternalServerError, err: err})
	}
	if err := putBytes(ctx, blobs, keys.metadata, rawMeta); err != nil {
		return fail(&publishFault{status: http.StatusBadGateway, err: err})
	}

	// Step 5: read everything back.
	stored, err := verifyPublished(ctx, blobs, keys, size, sum)
	if err != nil {
		return fail(&publishFault{status: http.StatusBadGateway, err: err})
	}
	if stored.KernelVersion != meta.KernelVersion ||
		stored.FirecrackerVersion != meta.FirecrackerVersion ||
		stored.EnvdVersion != meta.EnvdVersion {
		return fail(faultf(http.StatusBadGateway,
			"metadata.json read back from the store disagrees with what was published "+
				"(kernel %q/%q, firecracker %q/%q, envd %q/%q) — refusing to cache a version "+
				"triple in the database that the artifact does not carry",
			stored.KernelVersion, meta.KernelVersion,
			stored.FirecrackerVersion, meta.FirecrackerVersion,
			stored.EnvdVersion, meta.EnvdVersion))
	}
	if stored.PayloadName() != payload {
		return fail(faultf(http.StatusBadGateway,
			"metadata.json read back from the store names payload %q, not %q",
			stored.PayloadName(), payload))
	}

	// Step 6: the artifacts rows.
	total := size + int64(len(rawHeader)) + int64(len(rawMeta))
	rows := []store.ArtifactObject{
		{BuildID: spec.BuildID, Kind: payload, Key: keys.data, Size: size,
			SHA256: hex.EncodeToString(sum[:]), StoreKind: blobs.Name(), UploadedAt: spec.Now},
		{BuildID: spec.BuildID, Kind: payload + artifact.HeaderSuffix, Key: keys.header,
			Size: int64(len(rawHeader)), SHA256: sha256Hex(rawHeader), StoreKind: blobs.Name(), UploadedAt: spec.Now},
		{BuildID: spec.BuildID, Kind: artifact.NameMetadata, Key: keys.metadata,
			Size: int64(len(rawMeta)), SHA256: sha256Hex(rawMeta), StoreKind: blobs.Name(), UploadedAt: spec.Now},
	}
	for _, a := range rows {
		if err := st.RecordArtifactObject(ctx, a); err != nil {
			return fail(&publishFault{status: http.StatusInternalServerError, err: err})
		}
	}

	// Step 7: the ready mark, which makes the build bootable.
	ready := store.TemplateBuild{
		BuildID: spec.BuildID, ReadyAt: spec.Now, RootfsSize: size,
		RootfsSHA256:       hex.EncodeToString(sum[:]),
		TotalSize:          total,
		KernelVersion:      stored.KernelVersion,
		FirecrackerVersion: stored.FirecrackerVersion,
		EnvdVersion:        stored.EnvdVersion,
	}
	if err := st.MarkTemplateBuildReady(ctx, ready); err != nil {
		if errors.Is(err, store.ErrBuildNotBuilding) {
			// Not marked failed: the row is no longer this publish's to fail.
			return out, &publishFault{status: http.StatusConflict, err: err}
		}
		return fail(&publishFault{status: http.StatusInternalServerError, err: err})
	}

	final, err := st.TemplateBuildByID(ctx, spec.BuildID)
	if err != nil || final == nil {
		// The ready mark landed, so a failed read-back is not a publish failure.
		final = &store.TemplateBuild{BuildID: spec.BuildID, TemplateID: spec.TemplateID, OrgID: org,
			Status: "ready", RootfsSize: size, RootfsSHA256: ready.RootfsSHA256,
			TotalSize: total, CreatedAt: spec.Now, ReadyAt: spec.Now}
	}
	return publishOutcome{Build: final, Objects: rows}, nil
}
