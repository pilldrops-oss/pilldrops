//go:build linux

package main

import "golang.org/x/sys/unix"

// Applied in Go because dash, the guest /bin/sh, has no ulimit -u.
// No RLIMIT_AS (low VA caps kill pip and chromium) and no RLIMIT_CPU
// (the request timeout bounds execution).
const (
	limitNproc  = 2048  // fork-bomb guard
	limitNofile = 65536 // dev servers and bundlers hold many fds
)

// applyLimits sets rlimits on a just-started child; best-effort.
func applyLimits(pid int) {
	lim := func(resource int, v uint64) {
		_ = unix.Prlimit(pid, resource, &unix.Rlimit{Cur: v, Max: v}, nil)
	}
	lim(unix.RLIMIT_NPROC, limitNproc)
	lim(unix.RLIMIT_NOFILE, limitNofile)
}
