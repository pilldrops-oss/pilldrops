package templates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Source materialises one template into dir/<id>/rootfs.squashfs (plus an
// optional template.json), the layout the disk scan reads. It is called with
// no package lock held and must honour ctx, since a fetch is hundreds of megabytes.
// Injected rather than imported so internal/artifact stays out of the guest daemon.
type Source func(ctx context.Context, id, dir string) error

var (
	// ErrNotInStore is returned by a Source when the store has no such
	// template; any other error means the fetch failed. ResolveFetch maps the
	// first to ErrUnknownTemplate and the rest to ErrFetchFailed.
	ErrNotInStore = errors.New("templates: no such template in the artifact store")

	// ErrUnknownTemplate means no tier has this id.
	ErrUnknownTemplate = errors.New("templates: unknown template")

	// ErrFetchFailed means the template may exist but this node could not obtain it.
	ErrFetchFailed = errors.New("templates: fetching the template from the artifact store failed")

	// ErrFetchInProgress means this node is downloading the template right now
	// and the caller ran out of patience first. Unlike ErrFetchFailed nothing
	// is wrong: the download outlives the request that started it, so a later
	// create finds the template held. It exists so that answer can be told
	// apart from a failure by everything above this package.
	ErrFetchInProgress = errors.New("templates: the template is still being fetched from the artifact store")

	// errNoStoreTier means no source or no image directory; unlike
	// ErrNotInStore it is not worth a negative-cache entry.
	errNoStoreTier = errors.New("templates: no artifact-store tier configured")
)

// missTTL bounds store round trips to one per unknown id per window. Matches
// scanTTL because disk discovery is only that fresh anyway.
const missTTL = scanTTL

// fetchTimeout bounds one background download. Generous because the largest
// thing this fetches is a named snapshot — a memory image, a machine state and
// a root disk, measured at 1.6-1.8 GB on the reference fleet — and a bound
// short enough to cut that off would be the bug this file just fixed.
//
// When it is exceeded the fetch fails like any other, the miss is cached for
// missTTL, and the next create starts a new one. Objects that did land are
// already in the artifact cache under their verified names, so the retry
// resumes at object granularity rather than from zero.
const fetchTimeout = 30 * time.Minute

// remembered is one cached negative answer. The error itself is kept, not
// just the timestamp, or a store outage would be replayed as ErrNotInStore
// and reported to callers as a 400 for the rest of the window.
type remembered struct {
	at  time.Time
	err error
}

var src struct {
	mu    sync.Mutex
	fetch Source
	// inflight collapses concurrent fetches of the same id onto one download.
	inflight map[string]*fetchLead
	// current answers "which build should this id be serving", so a disk hit
	// can be checked against it instead of trusted blind.
	current CurrentBuild
	missed  map[string]remembered
	// now is injectable for tests.
	now func() time.Time
}

// SetSource installs the artifact-store tier; nil disables it. Installing
// clears the negative cache, since a new source is a new answer.
func SetSource(fn Source) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.fetch = fn
	src.missed = map[string]remembered{}
	src.inflight = map[string]*fetchLead{}
}

// HasSource reports whether ResolveFetch could answer for an id no local tier
// knows. The create gate needs this because Resolve never fetches, so a
// template held only in the shared store is unknown to it. Does not say
// whether the store holds a particular id.
func HasSource() bool {
	if Dir() == "" {
		return false
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	return src.fetch != nil
}

// srcNow reads the injectable clock. Call with src.mu held.
func srcNow() time.Time {
	if src.now != nil {
		return src.now()
	}
	return time.Now()
}

// fetchIntoDir asks the store for id. It returns no Template on purpose: the
// caller re-reads disk, so the scan stays the only tier that produces one.
func fetchIntoDir(ctx context.Context, id string) error {
	dir := Dir()
	if dir == "" {
		return errNoStoreTier
	}

	src.mu.Lock()
	fetch := src.fetch
	if fetch == nil {
		src.mu.Unlock()
		return errNoStoreTier
	}
	if m, ok := src.missed[id]; ok && srcNow().Sub(m.at) < missTTL {
		src.mu.Unlock()
		return m.err
	}
	if lead, ok := src.inflight[id]; ok {
		src.mu.Unlock()
		return waitFor(ctx, lead, id)
	}
	lead := &fetchLead{done: make(chan struct{})}
	if src.inflight == nil {
		src.inflight = map[string]*fetchLead{}
	}
	src.inflight[id] = lead
	src.mu.Unlock()

	// The download is detached from the caller that started it. It used to run
	// on the create's request context, and the control plane's node transport
	// gives up on a create after httpx.ResponseHeaderTimeout — which a
	// multi-gigabyte snapshot never fits into. Every give-up cancelled the
	// download and every retry started another, so a cold snapshot boot 503'd
	// for minutes and only converged on whatever objects happened to land
	// inside a 30-second window. Now one download runs to completion and every
	// later create waits on it or finds it done.
	go runFetch(context.WithoutCancel(ctx), lead, fetch, id, dir)
	return waitFor(ctx, lead, id)
}

// waitFor blocks on one in-flight fetch for as long as ctx allows. A caller
// that gives up first is told the fetch is still running, not that it failed:
// the download is no longer its to cancel.
func waitFor(ctx context.Context, lead *fetchLead, id string) error {
	select {
	case <-lead.done:
		return lead.err
	case <-ctx.Done():
		return fmt.Errorf("%w: %s", ErrFetchInProgress, id)
	}
}

// runFetch performs the download and publishes its answer to every waiter.
func runFetch(ctx context.Context, lead *fetchLead, fetch Source, id, dir string) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	err := fetch(ctx, id, dir)
	// A source that reports success but leaves nothing would never be cached
	// as a miss, so every later resolve would download again.
	if err == nil {
		if _, statErr := os.Stat(filepath.Join(dir, id, "rootfs.squashfs")); statErr != nil {
			err = errors.New("source reported success but left no rootfs.squashfs")
		}
	}

	src.mu.Lock()
	delete(src.inflight, id)
	if err != nil {
		if src.missed == nil {
			src.missed = map[string]remembered{}
		}
		src.missed[id] = remembered{at: srcNow(), err: err}
	}
	src.mu.Unlock()

	if err == nil {
		// The scan TTL must not hide what was just written.
		forceRescan()
	} else if !errors.Is(err, ErrNotInStore) {
		slog.Warn("fetching a template from the artifact store failed",
			"component", "templates", "template", id, "dir", dir, "err", err)
	}

	// Write err before closing done; the close is the happens-before edge.
	lead.err = err
	close(lead.done)
}

// fetchLead is one in-flight fetch and the answer its waiters will take.
type fetchLead struct {
	done chan struct{}
	err  error
}

// --- which build the image directory is holding ---------------------------
//
// A template id is not a version. `snapshot save app golden` twice publishes
// two builds under one id (store.SnapshotTemplateID carries no version), and
// `template build` does the same for an ordinary template. ResolveFetchTimed
// used to return on the first disk hit, so a node that already held v1 never
// fetched v2 and booted the stale image with every command reporting success.
//
// The fix is a build stamp written beside the image. The image directory stays
// keyed by template id and each fetch REPLACES what is there, so a name saved a
// hundred times still costs one image on disk; keying the directory by build id
// instead would fix the staleness and fill the disk. Older builds survive only
// in the artifact cache below, which is the tier that already has eviction and
// a size bound.

// buildStampName is the file, beside rootfs.squashfs, naming the build this
// node materialised. Written last, so a crash mid-fetch leaves a stamp that
// under-claims and the next create re-fetches.
const buildStampName = "build"

// HeldBuild returns the build id the image directory holds for id, or "" when
// nothing recorded one — an image the operator baked in place, or one fetched
// before stamping existed.
func HeldBuild(id string) string {
	dir := Dir()
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, id, buildStampName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// StampBuild records buildID as the build now materialised at <dir>/<id>.
func StampBuild(id, buildID string) error {
	dir := Dir()
	if dir == "" || buildID == "" {
		return nil
	}
	p := filepath.Join(dir, id, buildStampName)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(buildID), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CurrentBuild reports the build id the artifact index publishes for a template
// right now. "the index has never heard of this id" must come back as ("", nil):
// it is not a reason to distrust what is on disk.
type CurrentBuild func(ctx context.Context, id string) (string, error)

// SetCurrentBuild installs the index lookup; nil disables staleness checking.
// Separate from SetSource because a node can fetch without an index (tests) and
// the check is a pure addition to the fast path.
func SetCurrentBuild(fn CurrentBuild) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.current = fn
}

// heldIsStale reports whether the image on disk came from a DIFFERENT build
// than the one published now.
//
// It answers false for everything it cannot positively contradict — no index,
// an index error, an id the index does not know, an unstamped image. A node
// that cannot reach its index keeps booting what it holds rather than refusing
// every create; only a build it can SEE has changed makes it re-fetch. An
// unstamped image is one this node did not fetch (an operator baked it in
// place), and overwriting that from the index would take the "disk wins" rule
// away from the person holding the disk.
func heldIsStale(ctx context.Context, id string) bool {
	src.mu.Lock()
	cur := src.current
	src.mu.Unlock()
	if cur == nil {
		return false
	}
	held := HeldBuild(id)
	if held == "" {
		return false
	}
	now, err := cur(ctx, id)
	if err != nil || now == "" || now == held {
		return false
	}
	slog.Info("the image directory holds an older build of this template; re-fetching before boot",
		"component", "templates", "template", id, "held_build", held, "current_build", now)
	return true
}

// forceRescan drops the scan cache so the next read re-reads the directory.
func forceRescan() {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.lastScan = time.Time{}
}
