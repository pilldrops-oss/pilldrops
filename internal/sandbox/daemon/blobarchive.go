package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// BlobArchive keeps pause snapshots in an object store (S3, R2, MinIO, GCS),
// so a paused sandbox outlives its node and resumes wherever its record next
// lands, instead of only on a node that mounts the same network volume.
//
// Layout: pause/<id>/files/<relative path> per file, then pause/<id>/manifest.json
// written last. The manifest is what makes an archive exist: a Get before it
// lands, or after a Put died halfway, finds nothing rather than half a snapshot.
type BlobArchive struct {
	Store BlobStore
	// Lazy leaves the memory image in the bucket on Get, writing a mem.lazy
	// note where the file would be; the VM then boots with it served on
	// demand (vm.LazyMemSource). Only for an archive that stored per-block
	// checksums, which is what lets one block be fetched and verified alone.
	Lazy bool
	// RootfsCache, when set, keeps each shared root image fetched for a Get
	// under its build id (rfs-<digest>), so the next restore of any archive
	// taken against it links it instead of downloading it again. Verified
	// once, on its fetch.
	// ponytail: never evicted; one image per template build, add eviction if
	// a node sees many.
	RootfsCache string
	// CPUFeatures are this node's. Put records them and Get refuses a snapshot
	// taken on a CPU with instructions this one lacks: its guest has already
	// chosen code paths for them and would die with SIGILL here.
	CPUFeatures []string
}

type archiveManifest struct {
	// Dir is true when the snapshot was a directory rather than one file.
	Dir         bool     `json:"dir"`
	Files       []string `json:"files"` // slash-separated, relative to the snapshot
	CPUFeatures []string `json:"cpu_features,omitempty"`
	// Packed names the files stored as a block map (files/<rel>.header) rather
	// than whole: a memory image and an overlay only their data blocks, the
	// root image a reference to the one shared copy of it. An archive written
	// before packing has none, and reads whole.
	Packed []string `json:"packed,omitempty"`
	// PauseID is the id the paused record carries for this archive (PutPause);
	// a rehome takes the archive only when the two match.
	PauseID string `json:"pause_id,omitempty"`
	// Sums names the packed files whose per-block checksums are stored
	// (files/<rel>.sums), which a lazy read needs.
	Sums []string `json:"sums,omitempty"`
}

// LazyNote is what a mem.lazy file says: where the memory image stayed.
type LazyNote struct {
	Key    string `json:"key"`    // the packed data
	Header []byte `json:"header"` // its block map
	Sums   string `json:"sums"`   // its per-block checksums, 32 bytes each in stored order
}

// archiveBlockSize is the packing granularity: 2 MiB, as for a published capture.
const archiveBlockSize = 2 << 20

// packedFiles are stored as data blocks only; the root image is shared by digest.
var packedFiles = map[string]bool{"mem": true, "overlay.ext4": true}

func archiveKey(id string, parts ...string) string {
	return path.Join(append([]string{"pause", id}, parts...)...)
}

func (a *BlobArchive) manifest(ctx context.Context, id string) (archiveManifest, bool, error) {
	var m archiveManifest
	rc, err := a.Store.Get(ctx, archiveKey(id, "manifest.json"))
	if errors.Is(err, artifact.ErrNotFound) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	defer rc.Close()
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m); err != nil {
		return m, false, fmt.Errorf("archive %s: unreadable manifest: %w", id, err)
	}
	return m, true, nil
}

func (a *BlobArchive) Put(ctx context.Context, id, src string) error {
	return a.PutPause(ctx, id, src, "")
}

// PauseID reports the id the archive for id was stored under, ok=false when
// there is no archive.
func (a *BlobArchive) PauseID(ctx context.Context, id string) (string, bool, error) {
	m, ok, err := a.manifest(ctx, id)
	return m.PauseID, ok, err
}

// PutPause is Put recording pauseID in the manifest.
func (a *BlobArchive) PutPause(ctx context.Context, id, src, pauseID string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("archive put %s: %w", id, err)
	}
	// Retire the previous archive first: without its manifest it no longer
	// exists, so a Get during this Put finds nothing rather than a mix.
	if err := a.Delete(ctx, id); err != nil {
		return fmt.Errorf("archive put %s: retire the previous copy: %w", id, err)
	}
	m := archiveManifest{Dir: info.IsDir(), CPUFeatures: a.CPUFeatures, PauseID: pauseID}
	rootfsDigest := captureDigest(filepath.Join(src, "meta.json"))
	upload := func(file, rel string) error {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		var hdr *header.Header
		switch {
		case packedFiles[rel]:
			var sums [][sha256.Size]byte
			hdr, sums, err = artifact.PutPackedHashed(ctx, a.Store, archiveKey(id, "files", rel), archiveBuild(id),
				archiveBlockSize, f, 0)
			if err == nil {
				raw := make([]byte, 0, len(sums)*sha256.Size)
				for _, s := range sums {
					raw = append(raw, s[:]...)
				}
				err = a.Store.Put(ctx, archiveKey(id, "files", rel+".sums"), bytes.NewReader(raw), int64(len(raw)))
				m.Sums = append(m.Sums, rel)
			}
		case rel == "rootfs.squashfs" && rootfsDigest != "":
			hdr, err = a.putSharedRootfs(ctx, id, f, st.Size(), rootfsDigest)
		default:
			err = a.Store.Put(ctx, archiveKey(id, "files", rel), f, st.Size())
		}
		if err != nil {
			return err
		}
		if hdr != nil {
			raw, err := hdr.Marshal()
			if err != nil {
				return err
			}
			if err := a.Store.Put(ctx, archiveKey(id, "files", rel+".header"), bytes.NewReader(raw), int64(len(raw))); err != nil {
				return err
			}
			m.Packed = append(m.Packed, rel)
		}
		m.Files = append(m.Files, rel)
		return nil
	}
	if !info.IsDir() {
		err = upload(src, filepath.Base(src))
	} else {
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			return upload(p, filepath.ToSlash(rel))
		})
	}
	if err != nil {
		return fmt.Errorf("archive put %s: %w", id, err)
	}
	raw, _ := json.Marshal(m) //nolint:errcheck // a struct of a bool and strings
	if err := a.Store.Put(ctx, archiveKey(id, "manifest.json"), bytes.NewReader(raw), int64(len(raw))); err != nil {
		return fmt.Errorf("archive put %s: manifest: %w", id, err)
	}
	return nil
}

func (a *BlobArchive) Get(ctx context.Context, id, dst string) (bool, error) {
	m, ok, err := a.manifest(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	if len(a.CPUFeatures) > 0 {
		for _, f := range m.CPUFeatures {
			if !slices.Contains(a.CPUFeatures, f) {
				return false, fmt.Errorf("archive get %s: %w (lacks %s)", id, rt.ErrCPUTooPoor, f)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	_ = os.RemoveAll(dst)
	fail := func(err error) (bool, error) {
		_ = os.RemoveAll(dst) // never leave a half-written snapshot to restore
		return false, fmt.Errorf("archive get %s: %w", id, err)
	}
	for _, rel := range m.Files {
		if strings.Contains(rel, "..") {
			return fail(fmt.Errorf("manifest names %q outside the snapshot", rel))
		}
		target := dst
		if m.Dir {
			target = filepath.Join(dst, filepath.FromSlash(rel))
		}
		fetch := a.fetch
		if a.Lazy && rel == "mem" && m.Dir && slices.Contains(m.Sums, rel) {
			fetch = func(ctx context.Context, key, target string) error { return a.writeLazyNote(ctx, key, target) }
		} else if slices.Contains(m.Packed, rel) {
			fetch = func(ctx context.Context, key, target string) error { return a.fetchPacked(ctx, id, rel, key, target) }
		}
		if err := fetch(ctx, archiveKey(id, "files", rel), target); err != nil {
			return fail(err)
		}
	}
	return true, nil
}

// fetch writes one object to target, zero pages as holes: a memory image is
// mostly zeros and would otherwise land fully allocated.
func (a *BlobArchive) fetch(ctx context.Context, key, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := a.Store.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	n, err := io.Copy(&artifact.SparseWriter{F: f}, rc)
	if err == nil {
		err = f.Truncate(n)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (a *BlobArchive) Delete(ctx context.Context, id string) error {
	m, ok, err := a.manifest(ctx, id)
	if err != nil || !ok {
		return err
	}
	// Manifest first: from here the archive does not exist, whatever the
	// deletes below manage.
	if err := a.Store.Delete(ctx, archiveKey(id, "manifest.json")); err != nil && !errors.Is(err, artifact.ErrNotFound) {
		return err
	}
	for _, rel := range m.Files {
		keys := []string{archiveKey(id, "files", rel)}
		if slices.Contains(m.Packed, rel) {
			keys = append(keys, archiveKey(id, "files", rel+".header"))
		}
		if slices.Contains(m.Sums, rel) {
			keys = append(keys, archiveKey(id, "files", rel+".sums"))
		}
		for _, k := range keys {
			if err := a.Store.Delete(ctx, k); err != nil && !errors.Is(err, artifact.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// archiveBuild is the build id an archive's own packed data is recorded under.
func archiveBuild(id string) string { return "pause-" + id }

// fetchPacked rebuilds a packed file from its header, reading this archive's
// own data or the shared root image, as the header's builds say.
func (a *BlobArchive) fetchPacked(ctx context.Context, id, rel, key, target string) error {
	rc, err := a.Store.Get(ctx, key+".header")
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	_ = rc.Close()
	if err != nil {
		return err
	}
	hdr, err := header.Unmarshal(raw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if shared := sharedRootfsBuild(hdr); shared != "" && a.RootfsCache != "" {
		return a.linkCachedRootfs(ctx, hdr, shared, target)
	}
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	keyOf := func(build string) (string, error) {
		if build == archiveBuild(id) {
			return key, nil
		}
		if strings.HasPrefix(build, "rfs-") {
			return artifact.Key(build, artifact.NameRootfs)
		}
		return "", fmt.Errorf("archive %s: %s names build %q it has no key for", id, rel, build)
	}
	err = artifact.GetPacked(ctx, a.Store, keyOf, hdr, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// putSharedRootfs stores the root image once per digest, under the same key a
// published capture uses, and returns a header chaining this archive's copy
// onto it. An image already there was verified when stored; a new one is
// verified against the digest before anything points at it.
// ponytail: shared images are never deleted; count references if they pile up.
func (a *BlobArchive) putSharedRootfs(ctx context.Context, id string, f *os.File, size int64, digest string) (*header.Header, error) {
	shared := "rfs-" + digest
	key, err := artifact.Key(shared, artifact.NameRootfs)
	if err != nil {
		return nil, err
	}
	var sum [sha256.Size]byte
	if _, err := hex.Decode(sum[:], []byte(digest)); err != nil {
		return nil, err
	}
	stat, canStat := a.Store.(interface {
		Stat(context.Context, string) (artifact.ObjectInfo, error)
	})
	have := false
	if canStat {
		if info, err := stat.Stat(ctx, key); err == nil && info.Size == size {
			have = true
		}
	}
	if !have {
		h := sha256.New()
		if err := a.Store.Put(ctx, key, io.TeeReader(f, h), size); err != nil {
			return nil, err
		}
		if !bytes.Equal(h.Sum(nil), sum[:]) {
			_ = a.Store.Delete(context.WithoutCancel(ctx), key)
			return nil, fmt.Errorf("archive %s: the root image does not match the digest its capture records", id)
		}
	}
	self := archiveBuild(id)
	return &header.Header{
		BuildID: self, BaseBuildID: shared, BlockSize: archiveBlockSize, TotalSize: uint64(size),
		BuildIDs: []string{self, shared},
		Builds:   map[string]header.BuildData{self: {}, shared: {Size: uint64(size), SHA256: sum}},
		Mapping: []header.Entry{{StartBlock: 0,
			BlockCount: uint32((uint64(size) + archiveBlockSize - 1) / archiveBlockSize), BuildIndex: 1}},
	}, nil
}

// captureDigest is the hex root-image digest a capture's meta.json records,
// or "" when it has none.
func captureDigest(metaPath string) string {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var meta struct {
		RootfsDigest string `json:"rootfs_digest"`
	}
	if json.Unmarshal(raw, &meta) != nil {
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

// Portable: another node restores from this archive, so it holds a full
// capture (Daemon.PauseTimed) and comes back through Rehome.
func (*BlobArchive) Portable() bool { return true }

var _ SnapshotArchive = (*BlobArchive)(nil)

// writeLazyNote writes the mem.lazy note beside where the memory file would
// be, instead of downloading it.
func (a *BlobArchive) writeLazyNote(ctx context.Context, key, target string) error {
	rc, err := a.Store.Get(ctx, key+".header")
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	_ = rc.Close()
	if err != nil {
		return err
	}
	if _, err := header.Unmarshal(raw); err != nil {
		return err
	}
	note, _ := json.Marshal(LazyNote{Key: key, Header: raw, Sums: key + ".sums"}) //nolint:errcheck // fixed shape
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(target), "mem.lazy"), note, 0o600)
}

// LazySource is the memory image a mem.lazy note names, read block by block
// from the store and verified per block (artifact.ReadPackedBlock).
type LazySource struct {
	Store artifact.Ranger
	Key   string
	Hdr   *header.Header
	Sums  [][sha256.Size]byte
}

// OpenLazySource reads a note's block map and checksums from the store.
func OpenLazySource(ctx context.Context, store interface {
	artifact.Ranger
	Get(context.Context, string) (io.ReadCloser, error)
}, raw []byte) (*LazySource, error) {
	var n LazyNote
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("lazy memory note: %w", err)
	}
	hdr, err := header.Unmarshal(n.Header)
	if err != nil {
		return nil, err
	}
	rc, err := store.Get(ctx, n.Sums)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	if len(b)%sha256.Size != 0 {
		return nil, fmt.Errorf("lazy memory: %s is not a whole number of checksums", n.Sums)
	}
	sums := make([][sha256.Size]byte, len(b)/sha256.Size)
	for i := range sums {
		copy(sums[i][:], b[i*sha256.Size:])
	}
	return &LazySource{Store: store, Key: n.Key, Hdr: hdr, Sums: sums}, nil
}

// ReadBlock serves one block, retried: a transient bucket error must not
// strand a guest thread waiting on its fault.
func (s *LazySource) ReadBlock(ctx context.Context, off int64, p []byte) (bool, error) {
	b := uint64(off) / uint64(s.Hdr.BlockSize)
	var err error
	for try := 0; try < 4; try++ {
		var zero bool
		if zero, err = artifact.ReadPackedBlock(ctx, s.Store, s.Key, s.Hdr, s.Sums, b, p); err == nil {
			return zero, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Duration(try+1) * 500 * time.Millisecond):
		}
	}
	return false, err
}

// sharedRootfsBuild is the rfs-<digest> build a header maps wholly onto, or "".
func sharedRootfsBuild(hdr *header.Header) string {
	if len(hdr.Mapping) != 1 || int(hdr.Mapping[0].BuildIndex) >= len(hdr.BuildIDs) {
		return ""
	}
	if id := hdr.BuildIDs[hdr.Mapping[0].BuildIndex]; strings.HasPrefix(id, "rfs-") {
		return id
	}
	return ""
}

// linkCachedRootfs puts the shared root image at target, from the node's cache,
// fetching it into the cache first (verified) when it is not there yet.
func (a *BlobArchive) linkCachedRootfs(ctx context.Context, hdr *header.Header, shared, target string) error {
	cached := filepath.Join(a.RootfsCache, shared)
	if _, err := os.Stat(cached); err != nil {
		if err := os.MkdirAll(a.RootfsCache, 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(a.RootfsCache, ".fetch-"+shared+"-*")
		if err != nil {
			return err
		}
		keyOf := func(build string) (string, error) { return artifact.Key(build, artifact.NameRootfs) }
		err = artifact.GetPacked(ctx, a.Store, keyOf, hdr, tmp)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), cached)
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
			return fmt.Errorf("fetch shared root image %s: %w", shared, err)
		}
	}
	_ = os.Remove(target)
	if err := os.Link(cached, target); err == nil {
		return nil
	}
	return copyPath(cached, target)
}
