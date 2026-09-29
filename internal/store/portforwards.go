package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
)

// A port forward is one guest port published on a public TCP/UDP port of the
// platform's forwarding endpoint. The row is the whole feature: the ingress
// binds what it reads here, so a forward survives a control-plane restart, and
// DeleteSandbox drops the rows so a destroyed sandbox frees its public ports.

// PortForward is one row of port_forwards.
type PortForward struct {
	SandboxID  string `json:"sandbox_id"`
	GuestPort  int    `json:"guest_port"`
	PublicPort int    `json:"public_port"`
	TCP        bool   `json:"tcp"`
	UDP        bool   `json:"udp"`
	CreatedAt  int64  `json:"created_at"`
	// NodeID is filled only by ActivePortForwards, which joins sandboxes.
	NodeID string `json:"-"`
}

// ErrNoPublicPort means every port in the configured range is allocated.
var ErrNoPublicPort = errors.New("no free public port in the forwarding range")

// forwardAllocTries bounds the allocate-then-INSERT loop. Each attempt re-reads
// the taken set, so a loser of the UNIQUE race picks a different port next time.
const forwardAllocTries = 8

const portForwardCols = `sandbox_id, guest_port, public_port, tcp, udp, created_at`

// UpsertPortForward publishes guestPort, allocating a public port from
// [lo, hi] on first use. Re-exposing the same guest port keeps its public port
// and only moves the protocol set, which is what makes an endpoint stable.
func (s *Store) UpsertPortForward(ctx context.Context, f PortForward, lo, hi int) (PortForward, error) {
	res, err := s.exec(ctx,
		`UPDATE port_forwards SET tcp = ?, udp = ? WHERE sandbox_id = ? AND guest_port = ?`,
		f.TCP, f.UDP, f.SandboxID, f.GuestPort)
	if err != nil {
		return PortForward{}, err
	}
	if n, rErr := res.RowsAffected(); rErr == nil && n > 0 {
		return s.GetPortForward(ctx, f.SandboxID, f.GuestPort)
	}

	var last error
	for i := 0; i < forwardAllocTries; i++ {
		port, fErr := s.freePublicPort(ctx, lo, hi)
		if fErr != nil {
			return PortForward{}, fErr
		}
		f.PublicPort = port
		_, last = s.exec(ctx,
			`INSERT INTO port_forwards (`+portForwardCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
			f.SandboxID, f.GuestPort, f.PublicPort, f.TCP, f.UDP, f.CreatedAt)
		if last == nil {
			return f, nil
		}
	}
	// Every attempt lost the UNIQUE race (or the insert is failing for another
	// reason); surface the driver's own error rather than a made-up one.
	return PortForward{}, fmt.Errorf("allocating a public port failed %d times: %w", forwardAllocTries, last)
}

// freePublicPort picks a public port nothing holds. Random rather than lowest
// free: a port released a second ago is not immediately handed to the next
// sandbox, and concurrent allocators rarely pick the same one.
// ponytail: reads the taken set per attempt (a few hundred rows); a
// generate_series/recursive-CTE allocator would be one statement but two
// dialects of SQL.
func (s *Store) freePublicPort(ctx context.Context, lo, hi int) (int, error) {
	if lo < 1 || hi < lo || hi > 65535 {
		return 0, fmt.Errorf("forwarding range %d-%d is not a valid port range", lo, hi)
	}
	rows, err := s.query(ctx,
		`SELECT public_port FROM port_forwards WHERE public_port >= ? AND public_port <= ?`, lo, hi)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	taken := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, err
		}
		taken[p] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	free := hi - lo + 1 - len(taken)
	if free <= 0 {
		return 0, ErrNoPublicPort
	}
	// crypto/rand, not math/rand: a public port is handed to the internet, and
	// an allocator whose next choice is predictable is not worth the two lines
	// it saves (gosec G404).
	pick, err := rand.Int(rand.Reader, big.NewInt(int64(free)))
	if err != nil {
		return 0, err
	}
	k := int(pick.Int64())
	for p := lo; p <= hi; p++ {
		if taken[p] {
			continue
		}
		if k == 0 {
			return p, nil
		}
		k--
	}
	return 0, ErrNoPublicPort
}

// GetPortForward returns one row. ok is false when the sandbox has no such forward.
func (s *Store) GetPortForward(ctx context.Context, sandboxID string, guestPort int) (PortForward, error) {
	var f PortForward
	err := s.queryRow(ctx,
		`SELECT `+portForwardCols+` FROM port_forwards WHERE sandbox_id = ? AND guest_port = ?`,
		sandboxID, guestPort,
	).Scan(&f.SandboxID, &f.GuestPort, &f.PublicPort, &f.TCP, &f.UDP, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PortForward{}, nil
	}
	return f, err
}

// PortForwardsBySandbox returns one sandbox's forwards, guest-port ordered. Never nil.
func (s *Store) PortForwardsBySandbox(ctx context.Context, sandboxID string) ([]PortForward, error) {
	return s.queryPortForwards(ctx,
		`SELECT `+portForwardCols+`, '' FROM port_forwards WHERE sandbox_id = ? ORDER BY guest_port`, sandboxID)
}

// ActivePortForwards returns every forward whose sandbox still exists, with
// the owning node id. The join is what keeps the ingress from binding a port
// for a row whose sandbox row is gone.
func (s *Store) ActivePortForwards(ctx context.Context) ([]PortForward, error) {
	return s.queryPortForwards(ctx,
		`SELECT f.sandbox_id, f.guest_port, f.public_port, f.tcp, f.udp, f.created_at, s.node_id
		 FROM port_forwards f JOIN sandboxes s ON s.id = f.sandbox_id
		 ORDER BY f.public_port`)
}

// CountPortForwards counts one sandbox's forwards, for the plan limit.
func (s *Store) CountPortForwards(ctx context.Context, sandboxID string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM port_forwards WHERE sandbox_id = ?`, sandboxID).Scan(&n)
	return n, err
}

// DeletePortForward removes one forward and frees its public port. ok=false
// when the sandbox has no such forward, which the caller answers as a 404.
func (s *Store) DeletePortForward(ctx context.Context, sandboxID string, guestPort int) (bool, error) {
	res, err := s.exec(ctx, `DELETE FROM port_forwards WHERE sandbox_id = ? AND guest_port = ?`,
		sandboxID, guestPort)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) queryPortForwards(ctx context.Context, q string, args ...any) ([]PortForward, error) {
	return collectRows(ctx, s, q, args,
		func(rows *sql.Rows, f *PortForward) error {
			return rows.Scan(&f.SandboxID, &f.GuestPort, &f.PublicPort, &f.TCP, &f.UDP,
				&f.CreatedAt, &f.NodeID)
		})
}
