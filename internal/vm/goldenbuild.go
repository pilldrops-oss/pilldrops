package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/envdproto"
)

// ErrNoGolden means the template has no restorable golden snapshot yet. The
// create cold boots; it is not a restore failure.
var ErrNoGolden = errors.New("no golden snapshot for this template yet")

// goldenRetryAfter bounds how often a failed golden build is retried. Without it
// every create against the template would boot another golden VM.
const goldenRetryAfter = 10 * time.Minute

// goldenEnvdCheck refuses a golden build whose baked envd cannot bind in a VM
// that boots with no credential. Booting it anyway costs a 60 s stall and ends
// in the same refusal.
func goldenEnvdCheck(level int) error {
	if level < envdproto.VsockCredential {
		return fmt.Errorf("the template's baked envd is protocol level %d and a golden VM "+
			"needs level %d (the vsock credential channel); rebuild the template to "+
			"enable snapshot restore. Creates cold boot until then",
			level, envdproto.VsockCredential)
	}
	return nil
}

// maxJailerID is the longest --id the jailer accepts.
const maxJailerID = 64

// goldenVMID builds the golden VM's jailer --id. The jailer accepts only letters,
// digits and '-', so a template such as "python-3.11" cannot go in verbatim, and
// at most maxJailerID of them, so a long template name is cut and a hash of the
// whole name keeps two that share a prefix apart.
func goldenVMID(template, suffix string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, template)
	const prefix = "golden-"
	if room := maxJailerID - len(prefix) - len("-") - len(suffix); len(safe) > room {
		sum := sha256.Sum256([]byte(template))
		hash := hex.EncodeToString(sum[:4])
		safe = safe[:room-len(hash)-1] + "-" + hash
	}
	return prefix + safe + "-" + suffix
}

// goldenBuilds remembers, per template, whether a golden build is running and
// when the next one may start. A rebuilt template (new rootfs mtime) is always
// retried at once.
type goldenBuilds struct {
	mu    sync.Mutex
	state map[string]goldenBuildState
}

type goldenBuildState struct {
	running    bool
	rootfsMod  time.Time
	retryAfter time.Time
}

// start claims the right to build template's golden, or reports why not.
func (g *goldenBuilds) start(template string, rootfsMod, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state[template]
	if s.running {
		return false
	}
	if s.rootfsMod.Equal(rootfsMod) && now.Before(s.retryAfter) {
		return false
	}
	g.state[template] = goldenBuildState{running: true, rootfsMod: rootfsMod}
	return true
}

// finish records a build's outcome. A refusal holds until the rootfs changes; a
// failure holds for goldenRetryAfter.
func (g *goldenBuilds) finish(template string, err error, refused bool, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state[template]
	s.running = false
	switch {
	case refused:
		s.retryAfter = now.AddDate(100, 0, 0)
	case err != nil:
		s.retryAfter = now.Add(goldenRetryAfter)
	default:
		s.retryAfter = time.Time{}
	}
	g.state[template] = s
}
