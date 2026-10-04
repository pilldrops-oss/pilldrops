package runtime

import (
	"io/fs"
	"testing"
)

// The guest helpers are coreutils on one image and busybox on another, and
// the process runtime speaks Go's lowercase errno text; all three must land
// on the same sentinel or the same mistake gets a different status per runtime.
func TestFSKindReadsEveryRuntimesWording(t *testing.T) {
	for msg, want := range map[string]error{
		"stat: cannot statx 'x': No such file or directory":          ErrPathNotFound,
		"sh: 1: cd: can't cd to /root/x":                             ErrPathNotFound,
		"mkdir: cannot create directory 'a': File exists":            fs.ErrExist,
		"mv: cannot move 'a' to 'b': Directory not empty":            fs.ErrExist,
		"rm: cannot remove '/etc/x': Permission denied":              fs.ErrPermission,
		"mkdir: cannot create directory 'x': Read-only file system":  fs.ErrPermission,
		"mkdir: cannot create directory 'f/sub': Not a directory":    fs.ErrInvalid,
		"cat: d: Is a directory":                                     fs.ErrInvalid,
		"rm: refusing to remove '.' or '..' directory: skipping '.'": fs.ErrInvalid,
		"open /x: permission denied":                                 fs.ErrPermission,
		"rpc error: code = Unavailable desc = connection refused":    nil,
		"mv: cannot move 'a' to a subdirectory of itself, 'a/b/a'":   fs.ErrInvalid,
	} {
		if got := FSKind(msg); got != want {
			t.Errorf("FSKind(%q) = %v, want %v", msg, got, want)
		}
	}
}
