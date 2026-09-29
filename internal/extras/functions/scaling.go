package functions

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Each function is backed by a pool of replica sandboxes: invocations go to the
// least-loaded replica, a new one is created when all are at targetConcurrency
// and the pool is under max_replicas, and idle extras are reaped to warmReplicas.
const (
	defaultMaxReplicas = 3
	targetConcurrency  = 4
	warmReplicas       = 1
	reapIdle           = 45 * time.Second
	reapInterval       = 15 * time.Second
)

// replica is one live backing sandbox. Fields are guarded by fnPool.mu.
type replica struct {
	sandboxID string
	inflight  int
	lastUsed  time.Time
}

// fnPool is the live replica set for one function.
type fnPool struct {
	mu       sync.Mutex
	scaling  int // replicas currently being created
	replicas []*replica
}

func (r *Registry) poolFor(key string) *fnPool {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	p, ok := r.pools[key]
	if !ok {
		p = &fnPool{}
		r.pools[key] = p
	}
	return p
}

// AcquireReplica returns a sandbox to serve one invocation and whether it was
// freshly created. The caller must ReleaseReplica the id when the request finishes.
func (r *Registry) AcquireReplica(ctx context.Context, org, name string) (string, bool, error) {
	rec, ok, err := r.st.GetFunction(ctx, org, name)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, ErrNotFound
	}
	maxR := rec.MaxReplicas
	if maxR <= 0 {
		maxR = defaultMaxReplicas
	}
	key := org + "/" + name
	p := r.poolFor(key)

	p.mu.Lock()
	r.prune(ctx, p, rec)

	if rep := leastLoaded(p.replicas, targetConcurrency); rep != nil {
		rep.inflight++
		rep.lastUsed = time.Now()
		id := rep.sandboxID
		p.mu.Unlock()
		return id, false, nil
	}

	if len(p.replicas)+p.scaling < maxR {
		first := len(p.replicas) == 0 && p.scaling == 0
		p.scaling++
		p.mu.Unlock()
		id, derr := r.deploy(ctx, rec)
		p.mu.Lock()
		p.scaling--
		if derr != nil {
			p.mu.Unlock()
			return "", false, derr
		}
		p.replicas = append(p.replicas, &replica{sandboxID: id, inflight: 1, lastUsed: time.Now()})
		p.mu.Unlock()
		if first {
			r.setPrimary(ctx, org, name, id)
		}
		return id, true, nil
	}

	// At max and all busy: queue on the least-loaded replica rather than drop.
	rep := leastLoaded(p.replicas, 1<<30)
	if rep == nil {
		p.mu.Unlock()
		return "", false, errors.New("no replica available")
	}
	rep.inflight++
	rep.lastUsed = time.Now()
	id := rep.sandboxID
	p.mu.Unlock()
	return id, false, nil
}

// ReleaseReplica marks one in-flight request on a replica as finished.
func (r *Registry) ReleaseReplica(org, name, sandboxID string) {
	p := r.poolFor(org + "/" + name)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rep := range p.replicas {
		if rep.sandboxID == sandboxID {
			if rep.inflight > 0 {
				rep.inflight--
			}
			rep.lastUsed = time.Now()
			return
		}
	}
}

// ReplicaCount returns the live replica count for a function.
func (r *Registry) ReplicaCount(org, name string) int {
	p := r.poolFor(org + "/" + name)
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.replicas)
}

// leastLoaded returns the replica with the fewest in-flight requests below limit, or nil.
func leastLoaded(reps []*replica, limit int) *replica {
	var best *replica
	for _, rep := range reps {
		if rep.inflight < limit && (best == nil || rep.inflight < best.inflight) {
			best = rep
		}
	}
	return best
}

// prune drops dead replicas and seeds an empty pool from the persisted primary
// sandbox after a restart. Caller holds p.mu.
func (r *Registry) prune(ctx context.Context, p *fnPool, rec store.FunctionRecord) {
	alive := p.replicas[:0]
	for _, rep := range p.replicas {
		if _, live := r.d.Get(rep.sandboxID); live {
			alive = append(alive, rep)
		}
	}
	p.replicas = alive
	if len(p.replicas) == 0 && rec.SandboxID != "" {
		if _, live := r.d.Get(rec.SandboxID); live {
			p.replicas = append(p.replicas, &replica{sandboxID: rec.SandboxID, lastUsed: time.Now()})
		}
	}
}

// setPrimary records which replica the preview URL points at.
func (r *Registry) setPrimary(ctx context.Context, org, name, sandboxID string) {
	rec, ok, err := r.st.GetFunction(ctx, org, name)
	if err != nil || !ok || rec.SandboxID == sandboxID {
		return
	}
	rec.SandboxID = sandboxID
	rec.UpdatedAt = time.Now().Unix()
	_ = r.st.SaveFunction(ctx, rec)
}

// reap destroys idle extra replicas down to the warm baseline until Close.
func (r *Registry) reap() {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
		r.poolMu.Lock()
		pools := make(map[string]*fnPool, len(r.pools))
		for k, v := range r.pools {
			pools[k] = v
		}
		r.poolMu.Unlock()

		var doomed []string
		for _, p := range pools {
			p.mu.Lock()
			alive := p.replicas[:0]
			for _, rep := range p.replicas {
				if _, live := r.d.Get(rep.sandboxID); live {
					alive = append(alive, rep)
				}
			}
			p.replicas = alive
			now := time.Now()
			kept := make([]*replica, 0, len(p.replicas))
			for i, rep := range p.replicas {
				extra := i >= warmReplicas
				idle := rep.inflight == 0 && now.Sub(rep.lastUsed) > reapIdle
				if extra && idle {
					doomed = append(doomed, rep.sandboxID)
					continue
				}
				kept = append(kept, rep)
			}
			p.replicas = kept
			p.mu.Unlock()
		}

		// Destroy outside the pool locks.
		r.destroyReaped(doomed)
	}
}

// destroyReaped tears down replicas with bounded concurrency and a deadline.
func (r *Registry) destroyReaped(ids []string) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reapDestroyTimeout)
	defer cancel()
	sem := make(chan struct{}, reapDestroyConcurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := r.d.Destroy(ctx, id); err != nil {
				slog.ErrorContext(ctx, "reaping a replica failed",
					"component", "functions", "replica", id, "err", err)
			}
		}()
	}
	wg.Wait()
}

const (
	reapDestroyConcurrency = 4
	reapDestroyTimeout     = 2 * time.Minute
)
