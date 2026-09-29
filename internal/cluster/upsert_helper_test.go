package cluster

import "time"

// upsert registers a node in a test registry.
func upsert(r *Registry, id, url, key, zone string, labels map[string]string, version string, capacity Capacity, now time.Time) error {
	return r.Upsert(NodeUpsert{
		ID: id, URL: url, Key: key, Zone: zone,
		Labels: labels, Version: version, Cap: capacity,
	}, now)
}
