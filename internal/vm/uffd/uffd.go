//go:build linux

// Package uffd serves a Firecracker VM's guest memory on demand. Restored with
// the Uffd memory backend, Firecracker maps guest memory empty, registers it
// with a userfaultfd and hands that fd over a unix socket; every first touch of
// a page then faults here, and the page is filled from a Source. A memory image
// can so start running before it has been read, which is what lets a sandbox
// resume on a node that has not downloaded its memory yet.
//
// Blocks are filled whole on their first fault, then the rest prefetched in the
// background: once every block is in, no fault can reach this process again,
// and the VM no longer depends on it.
package uffd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Source supplies guest memory by offset in the memory image.
type Source interface {
	// ReadBlock fills p with the image's bytes at off. A block that is all
	// zeros may be reported with zero=true instead, leaving p untouched.
	ReadBlock(ctx context.Context, off int64, p []byte) (zero bool, err error)
}

// region is one guest memory mapping, as Firecracker sends it. v1.7 names the
// page size page_size_kib; later versions page_size, in bytes.
type region struct {
	Base       uint64 `json:"base_host_virt_addr"`
	Size       uint64 `json:"size"`
	Offset     uint64 `json:"offset"`
	PageSizeKB uint64 `json:"page_size_kib"`
	PageSize   uint64 `json:"page_size"`
}

// Server serves one VM's faults.
type Server struct {
	uffd      int
	regions   []region
	blockSize uint64
	src       Source

	mu     sync.Mutex
	filled map[uint64]bool // image offsets of blocks claimed for filling
	// copied counts blocks whose copy has finished. Completion is judged on
	// this, never on claims: closing the userfaultfd while a claimed block is
	// still being copied would let the waiting guest thread re-fault into an
	// unregistered range and read zeros where its memory should be.
	copied int
	total  int
	done   chan struct{}
	err    error

	// Faults counts faults served; Prefetched blocks filled by the prefetcher.
	Faults, Prefetched int
}

// Listen accepts Firecracker's handshake on the unix socket at path (created
// for uid, the jailed VMM's) and serves until every block is in or ctx ends.
// maxBytes is the VM's configured memory; a handshake describing more is
// refused. It returns once the handshake is done; Wait blocks until the memory
// is complete.
func Listen(ctx context.Context, path string, uid int, blockSize, maxBytes uint64, src Source) (<-chan *Server, error) {
	_ = os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chown(path, uid, uid); err != nil {
		_ = l.Close()
		return nil, err
	}
	ready := make(chan *Server, 1)
	go func() {
		defer func() { _ = l.Close() }()
		defer func() { _ = os.Remove(path) }()
		_ = l.SetDeadline(time.Now().Add(60 * time.Second))
		conn, err := l.AcceptUnix()
		if err != nil {
			ready <- &Server{err: fmt.Errorf("uffd: firecracker never connected: %w", err)}
			close(ready)
			return
		}
		s, err := handshake(conn, blockSize, maxBytes, src)
		_ = conn.Close()
		if err != nil {
			ready <- &Server{err: err}
			close(ready)
			return
		}
		ready <- s
		close(ready)
		s.serve(ctx)
	}()
	return ready, nil
}

// Err is the handshake's failure, if it failed.
func (s *Server) Err() error { return s.err }

func handshake(conn *net.UnixConn, blockSize, maxBytes uint64, src Source) (*Server, error) {
	buf := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("uffd: read handshake: %w", err)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return nil, fmt.Errorf("uffd: handshake carried no descriptor (%v)", err) //nolint:errorlint // err is nil when the parse succeeded but the count is wrong; %w would print %!w(<nil>)
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return nil, fmt.Errorf("uffd: handshake carried no userfaultfd (%v)", err) //nolint:errorlint // err is nil when the parse succeeded but the count is wrong; %w would print %!w(<nil>)
	}
	var regions []region
	if err := json.Unmarshal(buf[:n], &regions); err != nil {
		_ = unix.Close(fds[0])
		return nil, fmt.Errorf("uffd: guest memory layout %q does not parse: %w", buf[:n], err)
	}
	if err := checkRegions(regions, maxBytes); err != nil {
		_ = unix.Close(fds[0])
		return nil, err
	}
	s := &Server{uffd: fds[0], regions: regions, blockSize: blockSize, src: src,
		filled: map[uint64]bool{}, done: make(chan struct{})}
	for _, r := range regions {
		s.total += int((r.Size + blockSize - 1) / blockSize)
	}
	return s, nil
}

// checkRegions refuses a layout that is not at most maxBytes of guest memory.
// The VMM that sends it runs beside the guest, so it is read as untrusted: a
// region that wraps the address space, or regions adding up past the VM's
// memory, would have this process count and serve blocks without end.
func checkRegions(regions []region, maxBytes uint64) error {
	if len(regions) == 0 {
		return errors.New("uffd: the handshake described no guest memory")
	}
	var total uint64
	for i, r := range regions {
		if r.Size == 0 || r.Base+r.Size < r.Base || r.Offset+r.Size < r.Offset {
			return fmt.Errorf("uffd: guest memory region %d (%#x, %d bytes at offset %d) is empty or wraps",
				i, r.Base, r.Size, r.Offset)
		}
		total += r.Size
		if total < r.Size || total > maxBytes {
			return fmt.Errorf("uffd: guest memory regions add up to more than the VM's %d bytes", maxBytes)
		}
	}
	return nil
}

// Wait returns once every block is in (nil) or serving failed.
func (s *Server) Wait() error {
	<-s.done
	return s.err
}

const (
	// uffd_msg is 32 bytes; event at 0, pagefault.address at 16.
	msgSize            = 32
	eventPagefault     = 0x12
	uffdioCopy         = 0xC028AA03 // _IOWR(0xAA, 0x03, struct uffdio_copy)
	uffdioZeropage     = 0xC020AA04 // _IOWR(0xAA, 0x04, struct uffdio_zeropage)
	uffdioCopyModeNone = 0
)

func (s *Server) serve(ctx context.Context) {
	defer close(s.done)
	defer func() { _ = unix.Close(s.uffd) }()
	// The prefetcher fills every block the VM has not touched, so the VM stops
	// depending on this process as soon as it can.
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	prefetched := make(chan error, 1)
	go func() { prefetched <- s.prefetch(pctx) }()

	msg := make([]byte, msgSize)
	fds := []unix.PollFd{{Fd: int32(s.uffd), Events: unix.POLLIN}}
	for {
		if s.complete() {
			return
		}
		select {
		case err := <-prefetched:
			if err != nil {
				s.fail(err)
				return
			}
			prefetched = nil // keep serving faults already queued
		case <-ctx.Done():
			s.fail(ctx.Err())
			return
		default:
		}
		n, err := unix.Poll(fds, 100)
		if err != nil && !errors.Is(err, unix.EINTR) {
			s.fail(fmt.Errorf("uffd: poll: %w", err))
			return
		}
		if n == 0 {
			continue
		}
		if _, err := unix.Read(s.uffd, msg); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				continue
			}
			s.fail(fmt.Errorf("uffd: read event: %w", err))
			return
		}
		if msg[0] != eventPagefault {
			continue // remove/unmap events: nothing to fill
		}
		addr := *(*uint64)(unsafe.Pointer(&msg[16]))
		if err := s.fillAt(ctx, addr); err != nil {
			s.fail(err)
			return
		}
		s.mu.Lock()
		s.Faults++
		s.mu.Unlock()
	}
}

func (s *Server) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *Server) complete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copied >= s.total
}

// fillAt fills the block holding host address addr.
func (s *Server) fillAt(ctx context.Context, addr uint64) error {
	for _, r := range s.regions {
		if addr >= r.Base && addr < r.Base+r.Size {
			rel := (addr - r.Base) / s.blockSize * s.blockSize
			return s.fill(ctx, r, rel)
		}
	}
	return fmt.Errorf("uffd: fault at %#x is outside guest memory", addr)
}

// fill copies one block of region r, rel bytes into it, unless it is already in.
func (s *Server) fill(ctx context.Context, r region, rel uint64) error {
	off := r.Offset + rel
	s.mu.Lock()
	if s.filled[off] {
		s.mu.Unlock()
		return nil
	}
	s.filled[off] = true // claimed: a racing fill of the same block skips it
	s.mu.Unlock()

	n := s.blockSize
	if rel+n > r.Size {
		n = r.Size - rel
	}
	buf, err := unix.Mmap(-1, 0, int(n), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Munmap(buf) }()
	zero, err := s.src.ReadBlock(ctx, int64(off), buf)
	if err != nil {
		return fmt.Errorf("uffd: read memory at %d: %w", off, err)
	}
	dst := r.Base + rel
	if zero {
		err = ioctlRange(s.uffd, uffdioZeropage, dst, n)
	} else {
		err = ioctlCopy(s.uffd, dst, uint64(uintptr(unsafe.Pointer(&buf[0]))), n)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.copied++
	s.mu.Unlock()
	return nil
}

// prefetch fills every block in order; a block a fault already filled is skipped.
func (s *Server) prefetch(ctx context.Context) error {
	for _, r := range s.regions {
		for rel := uint64(0); rel < r.Size; rel += s.blockSize {
			if ctx.Err() != nil {
				return nil
			}
			if err := s.fill(ctx, r, rel); err != nil {
				return err
			}
			s.mu.Lock()
			s.Prefetched++
			s.mu.Unlock()
		}
	}
	return nil
}

// ioctlCopy is UFFDIO_COPY. EEXIST (some pages already in, a fault and the
// prefetcher racing on one block) counts as done.
func ioctlCopy(fd int, dst, src, n uint64) error {
	for done := uint64(0); done < n; {
		arg := struct {
			dst, src, len, mode uint64
			copied              int64
		}{dst + done, src + done, n - done, uffdioCopyModeNone, 0}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uffdioCopy, uintptr(unsafe.Pointer(&arg)))
		switch {
		case errno == 0:
			return nil
		case errno == syscall.EEXIST:
			return nil
		case errno == syscall.EAGAIN && arg.copied > 0:
			done += uint64(arg.copied)
		case errno == syscall.EAGAIN:
			continue
		default:
			return fmt.Errorf("uffd: UFFDIO_COPY at %#x: %w", dst+done, errno)
		}
	}
	return nil
}

// ioctlRange is UFFDIO_ZEROPAGE over [dst, dst+n).
func ioctlRange(fd int, req uintptr, dst, n uint64) error {
	arg := struct {
		start, len, mode uint64
		result           int64
	}{dst, n, 0, 0}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&arg)))
	if errno != 0 && errno != syscall.EEXIST {
		return fmt.Errorf("uffd: UFFDIO_ZEROPAGE at %#x: %w", dst, errno)
	}
	return nil
}

// FileSource reads a local memory image, the spike's source and the fallback.
// A read past the end of the file is the image's zero tail.
type FileSource struct{ F *os.File }

func (f FileSource) ReadBlock(_ context.Context, off int64, p []byte) (bool, error) {
	n, err := f.F.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	clear(p[n:])
	return false, nil
}
