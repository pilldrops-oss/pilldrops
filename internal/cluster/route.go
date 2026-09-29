package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RouteStore is the sandbox-to-node route catalog.
type RouteStore interface {
	Place(sandboxID, nodeID string) error
	Unplace(sandboxID string) error
	NodeFor(sandboxID string) (nodeID string, ok bool, err error)
	PlacementsOn(nodeID string) ([]string, error)
	UnplaceNode(nodeID string) (removed int, err error)
}

type memRouteStore struct {
	mu     sync.RWMutex
	routes map[string]string
}

func newMemRouteStore() *memRouteStore {
	return &memRouteStore{routes: map[string]string{}}
}

func (m *memRouteStore) Place(sandboxID, nodeID string) error {
	m.mu.Lock()
	m.routes[sandboxID] = nodeID
	m.mu.Unlock()
	return nil
}

func (m *memRouteStore) Unplace(sandboxID string) error {
	m.mu.Lock()
	delete(m.routes, sandboxID)
	m.mu.Unlock()
	return nil
}

func (m *memRouteStore) NodeFor(sandboxID string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.routes[sandboxID]
	return id, ok, nil
}

func (m *memRouteStore) PlacementsOn(nodeID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for sb, nid := range m.routes {
		if nid == nodeID {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (m *memRouteStore) UnplaceNode(nodeID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for sb, nid := range m.routes {
		if nid == nodeID {
			delete(m.routes, sb)
			n++
		}
	}
	return n, nil
}

// routesKey is a hash of sandbox id to node id.
const routesKey = "sandforge:routes"

type redisRouteStore struct {
	c *redis.Client
}

func NewRedisRouteStore(url string) (*redisRouteStore, error) {
	c, err := dialRedis(url)
	if err != nil {
		return nil, err
	}
	return &redisRouteStore{c: c}, nil
}

// rctx bounds one Redis round trip. A store call is on the request path, so a
// wedged Redis must fail fast rather than hold the caller.
func rctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

// dialRedis connects and proves the connection before handing it back: a store
// that cannot reach Redis must fail at construction, where the caller can still
// fall back, and not on the first placement.
func dialRedis(url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	c := redis.NewClient(opt)
	ctx, cancel := rctx()
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *redisRouteStore) Place(sandboxID, nodeID string) error {
	ctx, cancel := rctx()
	defer cancel()
	return r.c.HSet(ctx, routesKey, sandboxID, nodeID).Err()
}

func (r *redisRouteStore) Unplace(sandboxID string) error {
	ctx, cancel := rctx()
	defer cancel()
	return r.c.HDel(ctx, routesKey, sandboxID).Err()
}

func (r *redisRouteStore) NodeFor(sandboxID string) (string, bool, error) {
	ctx, cancel := rctx()
	defer cancel()
	id, err := r.c.HGet(ctx, routesKey, sandboxID).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (r *redisRouteStore) PlacementsOn(nodeID string) ([]string, error) {
	ctx, cancel := rctx()
	defer cancel()
	all, err := r.c.HGetAll(ctx, routesKey).Result()
	if err != nil {
		return nil, err
	}
	var out []string
	for sb, nid := range all {
		if nid == nodeID {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (r *redisRouteStore) UnplaceNode(nodeID string) (int, error) {
	ctx, cancel := rctx()
	defer cancel()
	all, err := r.c.HGetAll(ctx, routesKey).Result()
	if err != nil {
		return 0, err
	}
	var fields []string
	for sb, nid := range all {
		if nid == nodeID {
			fields = append(fields, sb)
		}
	}
	if len(fields) == 0 {
		return 0, nil
	}
	if err := r.c.HDel(ctx, routesKey, fields...).Err(); err != nil {
		return 0, err
	}
	return len(fields), nil
}

// reset clears the catalog; test-only.
func (r *redisRouteStore) reset() error {
	ctx, cancel := rctx()
	defer cancel()
	return r.c.Del(ctx, routesKey).Err()
}
