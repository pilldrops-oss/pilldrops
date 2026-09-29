//go:build !dev

package nodeboot

import (
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// DevBuild reports whether this binary was built with -tags dev.
const DevBuild = false

// unisolatedRuntime returns nil in a release build, which must not link
// internal/runtime/process at all. IsolationGuardError refuses to start on nil.
func unisolatedRuntime(string) rt.Runtime { return nil }
