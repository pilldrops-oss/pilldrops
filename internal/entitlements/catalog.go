package entitlements

import "sync/atomic"

// catalog is the operator-installed plan list; nil means the compiled presets apply.
var catalog atomic.Pointer[[]Plan]

// SetCatalog installs the live preset list. An empty list is ignored so a failed
// load cannot silently demote every tenant to the smallest preset.
func SetCatalog(ps []Plan) {
	if len(ps) == 0 {
		return
	}
	cp := append([]Plan(nil), ps...)
	catalog.Store(&cp)
}

// Reset drops the installed catalogue, restoring the compiled presets.
// Tests that call SetCatalog must defer this.
func Reset() { catalog.Store(nil) }
