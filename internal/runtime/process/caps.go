// Package process is the host-process Runtime: sandboxes are directories on the
// host and commands run via /bin/sh, with no isolation. Release builds do not
// import it; it stays untagged because many test suites use it as a fixture.
package process

import rt "github.com/pilldrops-oss/pilldrops/internal/runtime"

// DeclaredCaps is this runtime's capability declaration; Isolation is false.
func DeclaredCaps() rt.Caps { return rt.Caps{Isolation: false, Snapshot: true} }
