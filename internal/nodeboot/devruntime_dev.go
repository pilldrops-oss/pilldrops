//go:build dev

package nodeboot

import (
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
)

// DevBuild reports whether this binary was built with -tags dev. A constant,
// not an env lookup, so a release artifact cannot be switched into dev behaviour at run time.
const DevBuild = true

// unisolatedRuntime returns the process runtime: host processes, no boundary.
func unisolatedRuntime(baseDir string) rt.Runtime { return process.New(baseDir) }
