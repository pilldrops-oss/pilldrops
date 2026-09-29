package orchestrator

import (
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// upsert registers a node in a test registry without a mostly-zero struct literal at every call site.
func upsert(r *cluster.Registry, id, url, key, zone string, labels map[string]string, version string, capacity cluster.Capacity, now time.Time) error {
	return r.Upsert(cluster.NodeUpsert{
		ID: id, URL: url, Key: key, Zone: zone,
		Labels: labels, Version: version, Cap: capacity,
	}, now)
}
