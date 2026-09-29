package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// A directory snapshot goes into the bucket and comes back byte for byte on
// another path (another node), a second pause replaces it rather than mixing
// with it, and a delete makes it not exist.
func TestBlobArchiveRoundTrip(t *testing.T) {
	store, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a := &BlobArchive{Store: store}
	ctx := context.Background()

	src := t.TempDir()
	mem := append(make([]byte, 3<<20), []byte("guest-state")...) // mostly zero, like a memfile
	write := func(rel string, b []byte) {
		p := filepath.Join(src, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("mem.snap", mem)
	write("vm/state", []byte("v1"))
	write("old-only", []byte("gone after the second pause"))
	if err := a.Put(ctx, "sb-1", src); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(src, "old-only"))
	write("vm/state", []byte("v2"))
	if err := a.Put(ctx, "sb-1", src); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "restored")
	ok, err := a.Get(ctx, "sb-1", dst)
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "mem.snap")); !bytes.Equal(got, mem) {
		t.Fatalf("memfile came back as %d bytes, want %d identical", len(got), len(mem))
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "vm", "state")); string(got) != "v2" {
		t.Fatalf("state = %q, want the second pause's v2", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "old-only")); !os.IsNotExist(err) {
		t.Fatal("a file from the first pause survived into the second")
	}

	if err := a.Delete(ctx, "sb-1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Get(ctx, "sb-1", filepath.Join(t.TempDir(), "x")); ok || err != nil {
		t.Fatalf("Get after Delete = %v, %v; want nothing", ok, err)
	}
	if ok, err := a.Get(ctx, "sb-never", filepath.Join(t.TempDir(), "x")); ok || err != nil {
		t.Fatalf("Get of an id never archived = %v, %v", ok, err)
	}
}

// A snapshot taken on a CPU with instructions this node lacks is refused
// before anything is downloaded; one taken on a poorer CPU restores.
func TestBlobArchiveRefusesAPoorerCPU(t *testing.T) {
	store, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "snap")
	if err := os.WriteFile(src, []byte("mem"), 0o600); err != nil {
		t.Fatal(err)
	}
	haswell := &BlobArchive{Store: store, CPUFeatures: []string{"sse4_2", "avx", "avx2"}}
	ivy := &BlobArchive{Store: store, CPUFeatures: []string{"sse4_2", "avx"}}
	if err := haswell.Put(ctx, "sb-h", src); err != nil {
		t.Fatal(err)
	}
	if ok, err := ivy.Get(ctx, "sb-h", filepath.Join(t.TempDir(), "x")); ok || !errors.Is(err, rt.ErrCPUTooPoor) {
		t.Fatalf("an Ivy node took a Haswell snapshot: ok=%v err=%v", ok, err)
	}
	if err := ivy.Put(ctx, "sb-i", src); err != nil {
		t.Fatal(err)
	}
	if ok, err := haswell.Get(ctx, "sb-i", filepath.Join(t.TempDir(), "x")); !ok || err != nil {
		t.Fatalf("a Haswell node refused an Ivy snapshot: ok=%v err=%v", ok, err)
	}
}

// A capture directory archives packed: the memory image and overlay as their
// data blocks, the root image once per digest and shared between sandboxes,
// and every file restores byte for byte. Deleting an archive leaves the
// shared image for the others.
func TestBlobArchivePacksACapture(t *testing.T) {
	store, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rootfs := bytes.Repeat([]byte("squashfs|"), 50000)
	sum := sha256.Sum256(rootfs)
	mem := append(make([]byte, 6<<20), bytes.Repeat([]byte("page"), 1024)...) // 6 MiB of zeros, then data
	overlay := make([]byte, 8<<20)
	copy(overlay[3<<20:], "ext4-superblock")
	capture := func(dir string) {
		for name, b := range map[string][]byte{
			"mem": mem, "overlay.ext4": overlay, "rootfs.squashfs": rootfs, "state": []byte("vmstate"),
			"meta.json": []byte(`{"template":"base","rootfs_digest":"sha256:` + hex.EncodeToString(sum[:]) + `"}`),
		} {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	a := &BlobArchive{Store: store}
	srcA, srcB := t.TempDir(), t.TempDir()
	capture(srcA)
	capture(srcB)
	if err := a.Put(ctx, "sb-a", srcA); err != nil {
		t.Fatal(err)
	}
	sharedKey, _ := artifact.Key("rfs-"+hex.EncodeToString(sum[:]), artifact.NameRootfs)
	first, err := store.Stat(ctx, sharedKey)
	if err != nil {
		t.Fatalf("the root image was not stored under its digest: %v", err)
	}
	if err := a.Put(ctx, "sb-b", srcB); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.Stat(ctx, sharedKey); !again.Modified.Equal(first.Modified) {
		t.Error("a second sandbox's archive uploaded the shared root image again")
	}
	if info, err := store.Stat(ctx, archiveKey("sb-a", "files", "mem")); err != nil || info.Size >= int64(len(mem)) {
		t.Fatalf("the memory image was stored whole (%d bytes of %d): %v", info.Size, len(mem), err)
	}
	if _, err := store.Stat(ctx, archiveKey("sb-a", "files", "rootfs.squashfs")); err == nil {
		t.Error("the archive holds its own copy of the root image")
	}

	if err := a.Delete(ctx, "sb-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, sharedKey); err != nil {
		t.Fatal("deleting one archive removed the root image another still reads")
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if ok, err := a.Get(ctx, "sb-b", dst); !ok || err != nil {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	for name, want := range map[string][]byte{"mem": mem, "overlay.ext4": overlay, "rootfs.squashfs": rootfs} {
		if got, _ := os.ReadFile(filepath.Join(dst, name)); !bytes.Equal(got, want) {
			t.Errorf("%s restored as %d bytes, want %d identical", name, len(got), len(want))
		}
	}
}

// A record carries its archived pause only while paused: a save of a running
// sandbox clears it, so a rehome refuses the archive it moved on from.
func TestPauseArchiveOnlyWhilePaused(t *testing.T) {
	sb := &Sandbox{State: StatePaused, pauseArchive: "p1"}
	if got := pauseArchiveOf(sb); got != "p1" {
		t.Fatalf("paused: %q, want p1", got)
	}
	for _, st := range []State{StateRunning, StateResuming, StateStandby} {
		sb.State = st
		if got := pauseArchiveOf(sb); got != "" {
			t.Fatalf("%s: record carries archive %q; a rehome could roll it back", st, got)
		}
	}
}

// A lazy Get leaves the memory image in the store and writes a note; the
// source the note opens reads back, block by block, exactly the original.
func TestBlobArchiveLazyMemory(t *testing.T) {
	store, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := t.TempDir()
	mem := make([]byte, 5*archiveBlockSize+4096)
	copy(mem[archiveBlockSize:], bytes.Repeat([]byte("guest"), 50000))
	copy(mem[4*archiveBlockSize+100:], "tail-data")
	for name, b := range map[string][]byte{"mem": mem, "state": []byte("vmstate"), "overlay.ext4": make([]byte, 8192)} {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&BlobArchive{Store: store}).PutPause(ctx, "sb-l", src, "p1"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "capture")
	if ok, err := (&BlobArchive{Store: store, Lazy: true}).Get(ctx, "sb-l", dst); !ok || err != nil {
		t.Fatalf("lazy Get = %v, %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "mem")); !os.IsNotExist(err) {
		t.Fatal("a lazy Get downloaded the memory image")
	}
	note, err := os.ReadFile(filepath.Join(dst, "mem.lazy"))
	if err != nil {
		t.Fatalf("no mem.lazy note: %v", err)
	}
	ls, err := OpenLazySource(ctx, store, note)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 0, len(mem))
	for off := int64(0); off < int64(len(mem)); off += archiveBlockSize {
		p := make([]byte, min(int64(archiveBlockSize), int64(len(mem))-off))
		zero, err := ls.ReadBlock(ctx, off, p)
		if err != nil {
			t.Fatalf("block at %d: %v", off, err)
		}
		if zero {
			p = make([]byte, len(p))
		}
		got = append(got, p...)
	}
	if !bytes.Equal(got, mem) {
		t.Fatal("the lazily read memory differs from the original")
	}
}

// A shared root image is fetched once per node: a second restore links it
// from the cache, and still works after the bucket lost its copy.
func TestBlobArchiveCachesTheSharedRootfs(t *testing.T) {
	store, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rootfs := bytes.Repeat([]byte("image|"), 70000)
	sum := sha256.Sum256(rootfs)
	for _, id := range []string{"sb-1", "sb-2"} {
		dir := t.TempDir()
		for name, b := range map[string][]byte{"rootfs.squashfs": rootfs, "state": []byte("s"),
			"meta.json": []byte(`{"rootfs_digest":"sha256:` + hex.EncodeToString(sum[:]) + `"}`)} {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := (&BlobArchive{Store: store}).Put(ctx, id, dir); err != nil {
			t.Fatal(err)
		}
	}
	a := &BlobArchive{Store: store, RootfsCache: t.TempDir()}
	if ok, err := a.Get(ctx, "sb-1", filepath.Join(t.TempDir(), "one")); !ok || err != nil {
		t.Fatalf("first Get = %v, %v", ok, err)
	}
	sharedKey, _ := artifact.Key("rfs-"+hex.EncodeToString(sum[:]), artifact.NameRootfs)
	if err := store.Delete(ctx, sharedKey); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "two")
	if ok, err := a.Get(ctx, "sb-2", dst); !ok || err != nil {
		t.Fatalf("second Get, image gone from the bucket = %v, %v: it was not served from the cache", ok, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "rootfs.squashfs")); !bytes.Equal(got, rootfs) {
		t.Fatal("the cached root image differs")
	}
}
