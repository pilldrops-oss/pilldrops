package store

import (
	"context"
	"sort"
)

// Launch durations are reported by the client, which is untrusted, so each
// sample is clamped to a plausible range and the ETA is a median.
const (
	minLaunchSampleMs   = 1_000
	maxLaunchSampleMs   = 600_000
	maxLaunchSampleRows = 500
)

// RecordLaunchSample stores one observed launch duration.
func (s *Store) RecordLaunchSample(ctx context.Context, owner, template string, ms, at int64) error {
	if ms < minLaunchSampleMs {
		ms = minLaunchSampleMs
	}
	if ms > maxLaunchSampleMs {
		ms = maxLaunchSampleMs
	}
	_, err := s.exec(ctx,
		`INSERT INTO launch_timings (template, owner, ms, at) VALUES (?, ?, ?, ?)`,
		template, owner, ms, at)
	return err
}

// LaunchP50 returns the median launch duration for a template since a unix
// second (0 means all time) and the sample count. Returns (0, 0) with no samples,
// so callers must check n before treating the ETA as real.
func (s *Store) LaunchP50(ctx context.Context, template string, since int64) (int64, int) {
	rows, err := s.query(ctx, `
		SELECT ms FROM launch_timings
		WHERE template = ? AND at >= ?
		ORDER BY at DESC
		LIMIT ?`, template, since, maxLaunchSampleRows)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // best-effort: read loop below is what matters

	var samples []int64
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return 0, 0
		}
		samples = append(samples, ms)
	}
	if err := rows.Err(); err != nil {
		// A truncated read would yield a confident wrong median.
		return 0, 0
	}
	if len(samples) == 0 {
		return 0, 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2], len(samples)
}
