package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Operator console accounts. Kept separate from `users` (sandbox owners) so a
// tenant row can never collide with an operator row.

// AdminUser is a console account. PassHash carries its own PBKDF2 parameters.
type AdminUser struct {
	Email     string
	PassHash  string
	Role      string
	CreatedAt int64
	LastLogin int64
}

// ErrAdminExists is returned when the fleet has already been claimed.
var ErrAdminExists = errors.New("an administrator already exists")

// AdminCount reports how many console accounts exist; zero means first-run setup.
func (s *Store) AdminCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n)
	return n, err
}

// CreateFirstAdmin claims an unclaimed fleet. Count and insert share one
// transaction so two concurrent setups cannot both succeed.
func (s *Store) CreateFirstAdmin(ctx context.Context, email, passHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after a successful Commit

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrAdminExists
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO admin_users (email, pass_hash, role, created_at, last_login)
		 VALUES (?, ?, 'superadmin', ?, 0)`), email, passHash, now); err != nil {
		// A unique violation here is a lost race, not a database failure.
		return ErrAdminExists
	}
	return tx.Commit()
}

// CreateAdmin adds a further account. Authorization is the HTTP layer's job.
func (s *Store) CreateAdmin(ctx context.Context, email, passHash, role string) error {
	if role == "" {
		role = "admin"
	}
	_, err := s.exec(ctx,
		`INSERT INTO admin_users (email, pass_hash, role, created_at, last_login) VALUES (?, ?, ?, ?, 0)`,
		email, passHash, role, time.Now().Unix())
	if err != nil {
		return ErrAdminExists
	}
	return nil
}

// AdminByEmail looks up an account. Unknown address returns found=false, which
// the caller must treat exactly like a wrong password to avoid an email oracle.
func (s *Store) AdminByEmail(ctx context.Context, email string) (AdminUser, bool, error) {
	var a AdminUser
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT email, pass_hash, role, created_at, last_login FROM admin_users WHERE email = ?`),
		email).Scan(&a.Email, &a.PassHash, &a.Role, &a.CreatedAt, &a.LastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminUser{}, false, nil
	}
	if err != nil {
		return AdminUser{}, false, err
	}
	return a, true, nil
}

// TouchAdminLogin records a successful sign-in. Best-effort: a failed write
// must not fail the login.
func (s *Store) TouchAdminLogin(ctx context.Context, email string) {
	_, _ = s.exec(ctx, `UPDATE admin_users SET last_login = ? WHERE email = ?`, //nolint:errcheck // see comment
		time.Now().Unix(), email)
}

// UpdateAdminPassword rewrites a stored hash.
func (s *Store) UpdateAdminPassword(ctx context.Context, email, passHash string) error {
	_, err := s.exec(ctx, `UPDATE admin_users SET pass_hash = ? WHERE email = ?`, passHash, email)
	return err
}

// ListAdmins returns every console account. PassHash is never selected.
func (s *Store) ListAdmins(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT email, role, created_at, last_login FROM admin_users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only
	var out []AdminUser
	for rows.Next() {
		var a AdminUser
		if err := rows.Scan(&a.Email, &a.Role, &a.CreatedAt, &a.LastLogin); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
