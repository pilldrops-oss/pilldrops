package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Publishing a named snapshot. The same step order as publishBuild — building
// row, objects, metadata, read-back, artifact rows, ready mark last — but a
// snapshot has three data objects rather than one, and a build carrying two of
// them cannot restore, so the ready mark is the only thing that says all three
// landed.

// snapshotSource yields one of a snapshot's data objects. The publisher opens
// them one at a time: a memfile is gigabytes, and holding three streams open
// against a node while writing to the store buys nothing.
type snapshotSource func(ctx context.Context, name string) (io.ReadCloser, error)

// snapshotPublishSpec is one version's publish.
type snapshotPublishSpec struct {
	TemplateID string
	BuildID    string
	OrgID      string
	Meta       artifact.SnapshotMeta
	// Open streams one object by artifact name.
	Open snapshotSource
	Now  int64
}

// publishSnapshotBuild writes a snapshot's three objects, their block maps and
// metadata.json, then marks the build ready. The build id row must already
// exist as 'building' (the save handler writes it before answering 202, so a
// client polling the name sees it at once); on any failure here the row is
// marked failed and the objects are left for the store's own GC.
func publishSnapshotBuild(ctx context.Context, st *store.Store, blobs artifact.StorageProvider,
	spec snapshotPublishSpec) error {

	fail := func(err error) error {
		_ = st.FailTemplateBuild(context.WithoutCancel(ctx), spec.BuildID) //nolint:errcheck // best effort: a killed process cannot mark it either, so nothing may depend on it
		return err
	}

	var rows []store.ArtifactObject
	var total int64
	// primary is the object whose size and digest stand in for the build's in
	// template_builds. The memfile: it is the dominant one, and a build whose
	// memfile changed is a different machine.
	var primarySize int64
	var primarySum [sha256.Size]byte

	// Side by side: each object is one stream that the node gzips and this
	// side hashes, each bound to one core, so one after another cost the sum.
	// Read first: the root image is stored once per content digest, and its
	// publish has to know that digest before it decides whether to upload.
	rootfsDigest := captureRootfsDigest(ctx, spec)
	names := artifact.SnapshotObjects()
	results := make([]publishedObject, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if name == artifact.NameRootfs && rootfsDigest != "" {
				results[i], errs[i] = publishSharedRootfs(ctx, blobs, spec, rootfsDigest)
				return
			}
			results[i], errs[i] = publishSnapshotObject(ctx, blobs, spec, name)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fail(err)
	}
	for i, name := range names {
		r := results[i]
		total += r.size + int64(len(r.rawHeader))
		// A shared object is not this build's: a row would make the purge of
		// this build delete what every other capture of that image reads.
		if r.dataKey != "" {
			rows = append(rows, store.ArtifactObject{BuildID: spec.BuildID, Kind: name, Key: r.dataKey, Size: r.size,
				SHA256: hex.EncodeToString(r.sum[:]), StoreKind: blobs.Name(), UploadedAt: spec.Now})
		}
		rows = append(rows,
			store.ArtifactObject{BuildID: spec.BuildID, Kind: name + artifact.HeaderSuffix, Key: r.headerKey,
				Size: int64(len(r.rawHeader)), SHA256: sha256Hex(r.rawHeader), StoreKind: blobs.Name(), UploadedAt: spec.Now})
		if name == artifact.NameMemfile {
			primarySize, primarySum = r.size, r.sum
		}
	}

	metaKey, err := artifact.Key(spec.BuildID, artifact.NameMetadata)
	if err != nil {
		return fail(err)
	}
	snapMeta := spec.Meta
	meta := artifact.Metadata{
		BuildID:      spec.BuildID,
		TemplateID:   spec.TemplateID,
		Payload:      artifact.PayloadSnapshot,
		Snapshot:     &snapMeta,
		RootfsSize:   primarySize,
		RootfsSHA256: hex.EncodeToString(primarySum[:]),
		CreatedUnix:  spec.Now,
	}
	rawMeta, err := meta.Marshal()
	if err != nil {
		return fail(err)
	}
	if err := putBytes(ctx, blobs, metaKey, rawMeta); err != nil {
		return fail(err)
	}
	stored, err := readObject(ctx, blobs, metaKey, artifact.MaxMetadataBytes)
	if err != nil {
		return fail(err)
	}
	readBack, err := artifact.ParseMetadata(stored)
	if err != nil {
		return fail(err)
	}
	if readBack.PayloadName() != artifact.PayloadSnapshot || readBack.Snapshot == nil {
		return fail(fmt.Errorf("metadata.json read back from the store does not describe a snapshot "+
			"(payload %q, snapshot block present: %v) — a node would try to boot it as a template",
			readBack.PayloadName(), readBack.Snapshot != nil))
	}
	total += int64(len(rawMeta))
	rows = append(rows, store.ArtifactObject{BuildID: spec.BuildID, Kind: artifact.NameMetadata,
		Key: metaKey, Size: int64(len(rawMeta)), SHA256: sha256Hex(rawMeta),
		StoreKind: blobs.Name(), UploadedAt: spec.Now})

	for _, a := range rows {
		if err := st.RecordArtifactObject(ctx, a); err != nil {
			return fail(err)
		}
	}
	if err := st.MarkTemplateBuildReady(ctx, store.TemplateBuild{
		BuildID: spec.BuildID, ReadyAt: spec.Now, RootfsSize: primarySize,
		RootfsSHA256: hex.EncodeToString(primarySum[:]), TotalSize: total,
	}); err != nil {
		if errors.Is(err, store.ErrBuildNotBuilding) {
			// The row is no longer this publish's to fail.
			return err
		}
		return fail(err)
	}
	slog.Info("published a named snapshot to the shared artifact store", "component", "artifact",
		"template", spec.TemplateID, "build", spec.BuildID, "bytes", total,
		"runtime", spec.Meta.Runtime, "source", spec.Meta.SourceSandbox)
	return nil
}

// purgeBuild drops a build from the index and removes its objects. The index
// goes first, so a create can never resolve to bytes that are being deleted.
// Object removal is best effort: a leaked object costs disk, a half-deleted
// index costs every create of the template.
func purgeBuild(ctx context.Context, st *store.Store, blobs artifact.StorageProvider, buildID string) error {
	objs, err := st.DeleteBuild(ctx, buildID)
	if err != nil {
		return err
	}
	if blobs == nil {
		return nil
	}
	for _, o := range objs {
		if err := blobs.Delete(ctx, o.Key); err != nil && !errors.Is(err, artifact.ErrNotFound) {
			slog.Warn("a snapshot object could not be removed from the artifact store; it is no "+
				"longer indexed and occupies disk until the store is swept",
				"component", "artifact", "build", buildID, "key", o.Key, "err", err)
		}
	}
	return nil
}

// publishedObject is what one object's publish leaves for the build's rows.
type publishedObject struct {
	dataKey, headerKey string
	size               int64
	sum                [sha256.Size]byte
	rawHeader          []byte
}

// publishSnapshotObject streams one object of a snapshot into the store, then
// writes its block map and reads its size back.
func publishSnapshotObject(ctx context.Context, blobs artifact.StorageProvider,
	spec snapshotPublishSpec, name string) (publishedObject, error) {
	dataKey, err := artifact.Key(spec.BuildID, name)
	if err != nil {
		return publishedObject{}, err
	}
	headerKey, err := artifact.HeaderKey(spec.BuildID, name)
	if err != nil {
		return publishedObject{}, err
	}

	body, err := spec.Open(ctx, name)
	if err != nil {
		return publishedObject{}, fmt.Errorf("read %s of snapshot build %s: %w", name, spec.BuildID, err)
	}
	// A memory image and a disk overlay are mostly zero blocks: stored packed,
	// only the blocks holding data travel to the store and back out to a node.
	if name == artifact.NameMemfile || name == artifact.NameOverlay {
		return publishPacked(ctx, blobs, spec, name, dataKey, headerKey, body)
	}
	hr := &hashingReader{src: body, sum: sha256.New()}
	putErr := blobs.Put(ctx, dataKey, hr, -1)
	_ = body.Close() //nolint:errcheck // the transfer is over either way; Put's error is the one that matters
	if putErr != nil {
		return publishedObject{}, fmt.Errorf("writing %s: %w", dataKey, putErr)
	}
	size, sum := hr.n, hr.digest()
	// <= 0, not == 0: the uint64 conversion below turns a negative length
	// into an enormous one, and a block map claiming 16 exabytes is a worse
	// failure than an empty object. hashingReader cannot produce one, and
	// the guard is what makes that a property rather than a hope.
	if size <= 0 {
		return publishedObject{}, fmt.Errorf("%s of snapshot build %s is empty: a machine cannot be "+
			"restored from a zero-byte %s", name, spec.BuildID, name)
	}

	hdr, err := header.New(spec.BuildID, artifactBlockSize, uint64(size), sum, spec.Now)
	if err != nil {
		return publishedObject{}, err
	}
	rawHeader, err := hdr.Marshal()
	if err != nil {
		return publishedObject{}, err
	}
	if err := putBytes(ctx, blobs, headerKey, rawHeader); err != nil {
		return publishedObject{}, err
	}
	// Read back: the node cache refuses an object whose stored size does not
	// match its header, and finding that out on the first cold create is worse.
	info, err := blobs.Stat(ctx, dataKey)
	if err != nil {
		return publishedObject{}, fmt.Errorf("%s is not readable back from the store: %w", dataKey, err)
	}
	if info.Size != size {
		return publishedObject{}, fmt.Errorf("the store holds %d bytes under %s but %d were written",
			info.Size, dataKey, size)
	}

	return publishedObject{dataKey: dataKey, headerKey: headerKey, size: size, sum: sum, rawHeader: rawHeader}, nil
}

// publishPacked stores one object packed (artifact.PutPacked) and reads its
// stored size back, as the whole-object path does.
func publishPacked(ctx context.Context, blobs artifact.StorageProvider, spec snapshotPublishSpec,
	name, dataKey, headerKey string, body io.ReadCloser) (publishedObject, error) {
	hdr, err := artifact.PutPacked(ctx, blobs, dataKey, spec.BuildID, artifactBlockSize, body, spec.Now)
	_ = body.Close() //nolint:errcheck // the transfer is over either way; the put's error is the one that matters
	if err != nil {
		return publishedObject{}, fmt.Errorf("writing %s: %w", dataKey, err)
	}
	if hdr.TotalSize == 0 {
		return publishedObject{}, fmt.Errorf("%s of snapshot build %s is empty: a machine cannot be "+
			"restored from a zero-byte %s", name, spec.BuildID, name)
	}
	rawHeader, err := hdr.Marshal()
	if err != nil {
		return publishedObject{}, err
	}
	if err := putBytes(ctx, blobs, headerKey, rawHeader); err != nil {
		return publishedObject{}, err
	}
	stored := hdr.Builds[spec.BuildID]
	info, err := blobs.Stat(ctx, dataKey)
	if err != nil {
		return publishedObject{}, fmt.Errorf("%s is not readable back from the store: %w", dataKey, err)
	}
	if uint64(info.Size) != stored.Size {
		return publishedObject{}, fmt.Errorf("the store holds %d bytes under %s but %d were written",
			info.Size, dataKey, stored.Size)
	}
	return publishedObject{dataKey: dataKey, headerKey: headerKey, size: int64(stored.Size),
		sum: stored.SHA256, rawHeader: rawHeader}, nil
}

// captureRootfsDigest reads the root image digest the node recorded in the
// capture ("sha256:<hex>"), or "" when it recorded none, which publishes the
// root image whole as before.
func captureRootfsDigest(ctx context.Context, spec snapshotPublishSpec) string {
	body, err := spec.Open(ctx, artifact.NameSnapMeta)
	if err != nil {
		return ""
	}
	defer func() { _ = body.Close() }() //nolint:errcheck // read-only, fully consumed
	var meta struct {
		RootfsDigest string `json:"rootfs_digest"`
	}
	if json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&meta) != nil {
		return ""
	}
	d, ok := strings.CutPrefix(meta.RootfsDigest, "sha256:")
	if !ok || len(d) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(d); err != nil {
		return ""
	}
	return strings.ToLower(d)
}

// publishSharedRootfs stores the root image once per content digest, as build
// "rfs-<digest>", and writes this build's rootfs header pointing at it. Every
// capture of one template build shares one copy, where each used to upload the
// same 200+ MB again. An existing copy was verified against its digest when it
// was stored; a new one is verified before anything points at it.
// ponytail: shared images are never deleted; count references if they pile up.
func publishSharedRootfs(ctx context.Context, blobs artifact.StorageProvider, spec snapshotPublishSpec,
	digest string) (publishedObject, error) {
	shared := "rfs-" + digest
	sharedKey, err := artifact.Key(shared, artifact.NameRootfs)
	if err != nil {
		return publishedObject{}, err
	}
	headerKey, err := artifact.HeaderKey(spec.BuildID, artifact.NameRootfs)
	if err != nil {
		return publishedObject{}, err
	}
	var sum [sha256.Size]byte
	_, _ = hex.Decode(sum[:], []byte(digest)) //nolint:errcheck // validated by captureRootfsDigest
	info, err := blobs.Stat(ctx, sharedKey)
	size := info.Size
	if errors.Is(err, artifact.ErrNotFound) {
		body, oerr := spec.Open(ctx, artifact.NameRootfs)
		if oerr != nil {
			return publishedObject{}, fmt.Errorf("read the root image of snapshot build %s: %w", spec.BuildID, oerr)
		}
		hr := &hashingReader{src: body, sum: sha256.New()}
		err = blobs.Put(ctx, sharedKey, hr, -1)
		_ = body.Close() //nolint:errcheck // the put's error is the one that matters
		if err != nil {
			return publishedObject{}, fmt.Errorf("writing %s: %w", sharedKey, err)
		}
		if hr.digest() != sum {
			_ = blobs.Delete(context.WithoutCancel(ctx), sharedKey) //nolint:errcheck // best effort: it is wrong either way
			return publishedObject{}, fmt.Errorf("the root image of snapshot build %s does not match the digest "+
				"its capture records, so it was not stored as that image", spec.BuildID)
		}
		size = hr.n
	} else if err != nil {
		return publishedObject{}, err
	}
	if size <= 0 {
		return publishedObject{}, fmt.Errorf("the shared root image %s is empty", sharedKey)
	}
	blocks := (uint64(size) + artifactBlockSize - 1) / artifactBlockSize
	hdr := &header.Header{
		BuildID: spec.BuildID, BaseBuildID: shared, BlockSize: artifactBlockSize, TotalSize: uint64(size),
		BuildIDs:    []string{spec.BuildID, shared},
		Builds:      map[string]header.BuildData{spec.BuildID: {}, shared: {Size: uint64(size), SHA256: sum}},
		Mapping:     []header.Entry{{StartBlock: 0, BlockCount: uint32(blocks), BuildIndex: 1}},
		CreatedUnix: spec.Now,
	}
	rawHeader, err := hdr.Marshal()
	if err != nil {
		return publishedObject{}, err
	}
	if err := putBytes(ctx, blobs, headerKey, rawHeader); err != nil {
		return publishedObject{}, err
	}
	return publishedObject{headerKey: headerKey, size: 0, sum: sum, rawHeader: rawHeader}, nil
}
