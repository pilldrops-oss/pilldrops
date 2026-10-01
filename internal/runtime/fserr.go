package runtime

import (
	"io/fs"
	"strings"
)

// fsFailures maps what coreutils, busybox, sh and Go's os package say about a
// path to the sentinel an HTTP caller can act on. Matched lowercased because
// coreutils capitalises ("No such file or directory") and Go does not.
var fsFailures = []struct {
	says string
	kind error
}{
	{"no such file or directory", ErrPathNotFound},
	{"can't cd to", ErrPathNotFound},
	{"file exists", fs.ErrExist},
	{"directory not empty", fs.ErrExist},
	{"permission denied", fs.ErrPermission},
	{"operation not permitted", fs.ErrPermission},
	{"read-only file system", fs.ErrPermission},
	{"not a directory", fs.ErrInvalid},
	{"is a directory", fs.ErrInvalid},
	{"subdirectory of itself", fs.ErrInvalid},
	{"are the same file", fs.ErrInvalid},
	{"refusing to remove", fs.ErrInvalid},
}

// FSKind names the sentinel a sandbox-side file operation's failure message
// means: ErrPathNotFound, fs.ErrExist, fs.ErrPermission or fs.ErrInvalid, so
// the API can answer 404/409/403/400 instead of blaming the node with a 503.
// nil when the message is not one it recognises.
func FSKind(msg string) error {
	lower := strings.ToLower(msg)
	for _, f := range fsFailures {
		if strings.Contains(lower, f.says) {
			return f.kind
		}
	}
	return nil
}
