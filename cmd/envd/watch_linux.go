//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// runWatch is `envd watch <dir>`: it prints a JSON line per change directly in
// dir (not recursive) until dir goes away or stdout closes. The node runs it
// through an ordinary exec, as the sandbox user, so it sees what that user can.
func runWatch(args []string, out io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: envd watch <dir>")
		return 2
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "envd watch: %v\n", err)
		return 1
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envd watch: inotify: %v\n", err)
		return 1
	}
	defer func() { _ = unix.Close(fd) }()

	const mask = unix.IN_CREATE | unix.IN_MODIFY | unix.IN_DELETE |
		unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB |
		unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR
	if _, err := unix.InotifyAddWatch(fd, dir, mask); err != nil {
		fmt.Fprintf(os.Stderr, "envd watch: %s: %v\n", dir, err)
		return 1
	}
	if emitWatch(out, "ready", dir) != nil {
		return 0
	}

	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "envd watch: read: %v\n", err)
			return 1
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameBytes := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
			off += unix.SizeofInotifyEvent + int(ev.Len)

			name := dir
			if i := bytes.IndexByte(nameBytes, 0); i >= 0 {
				nameBytes = nameBytes[:i]
			}
			if len(nameBytes) > 0 {
				name = filepath.Join(dir, string(nameBytes))
			}
			if ev.Mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
				_ = emitWatch(out, "remove", dir)
				return 0
			}
			if op := watchOp(ev.Mask); op != "" {
				if emitWatch(out, op, name) != nil {
					return 0
				}
			}
		}
	}
}

// watchOp names an inotify mask the way the SDKs name file events.
func watchOp(mask uint32) string {
	switch {
	case mask&unix.IN_CREATE != 0:
		return "create"
	case mask&unix.IN_MODIFY != 0:
		return "write"
	case mask&unix.IN_DELETE != 0:
		return "remove"
	case mask&(unix.IN_MOVED_FROM|unix.IN_MOVED_TO) != 0:
		return "rename"
	case mask&unix.IN_ATTRIB != 0:
		return "chmod"
	}
	return ""
}
