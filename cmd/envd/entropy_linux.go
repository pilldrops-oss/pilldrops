//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seedEntropy credits the kernel entropy pool so getrandom() does not block:
// the guest has no virtio-rng, and anything seeding a CSPRNG at startup would
// hang. Seed quality is traded for liveness.
func seedEntropy() {
	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()

	// Mirrors struct rand_pool_info { int entropy_count; int buf_size; char buf[]; }.
	const n = 512
	var info struct {
		entropyCount int32
		bufSize      int32
		buf          [n]byte
	}
	if u, uerr := os.Open("/dev/urandom"); uerr == nil {
		_, _ = u.Read(info.buf[:])
		u.Close()
	}
	binary.LittleEndian.PutUint64(info.buf[:8], uint64(time.Now().UnixNano()))
	info.bufSize = n
	info.entropyCount = n * 8 // full credit so the pool crosses the init threshold

	for i := 0; i < 4; i++ {
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(),
			uintptr(unix.RNDADDENTROPY), uintptr(unsafe.Pointer(&info))); errno != 0 {
			return
		}
	}
}
