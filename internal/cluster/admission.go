package cluster

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Admission holds an org's in-flight creates in Redis, so the concurrency check
// is one atomic script instead of a Postgres transaction serialised on an
// advisory lock per org. The live count still comes from Postgres, the source
// of truth; only the pending creates and the check-and-claim move here.
type Admission struct{ c *redis.Client }

// NewAdmission opens the Redis at url.
func NewAdmission(url string) (*Admission, error) {
	c, err := dialRedis(url)
	if err != nil {
		return nil, err
	}
	return &Admission{c: c}, nil
}

const (
	// admissionPendingTTL is how long an unreleased claim holds its place, the
	// same window the Postgres slots use.
	admissionPendingTTL = 180 * time.Second
	// admissionReleaseGrace keeps a released claim counted a little longer, to
	// cover the gap between the node writing the sandbox row and a concurrent
	// check's live count seeing it. Errs toward refusing, never over quota.
	admissionReleaseGrace = 5 * time.Second
)

// reserveScript: drop expired claims, then claim a place if live + pending < cap.
var reserveScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if tonumber(ARGV[4]) + redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[5]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
redis.call('EXPIRE', KEYS[1], 600)
return 1`)

func admissionKey(org string) string { return "sandforge:admit:" + org }

// Reserve claims one create for org when live plus in-flight creates are below cap.
func (a *Admission) Reserve(ctx context.Context, org, slot string, live, cap int, now time.Time) (bool, error) {
	n, err := reserveScript.Run(ctx, a.c, []string{admissionKey(org)},
		now.UnixNano(), now.Add(admissionPendingTTL).UnixNano(), slot, live, cap).Int()
	return n == 1, err
}

// Release lets a claim lapse after admissionReleaseGrace. XX: never re-adds one
// that already expired.
func (a *Admission) Release(ctx context.Context, org, slot string, now time.Time) error {
	return a.c.ZAddXX(ctx, admissionKey(org), redis.Z{
		Score: float64(now.Add(admissionReleaseGrace).UnixNano()), Member: slot,
	}).Err()
}
