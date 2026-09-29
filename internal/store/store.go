// Package store is sandboxd's persistence layer, backed by SQLite or Postgres
// depending on the DSN passed to Open.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver name "pgx"
	_ "modernc.org/sqlite"
)

// Ping verifies the database connection is alive, for deep health checks.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Store is a database handle. Safe for concurrent use.
type Store struct {
	db      *sql.DB
	dialect dialect
	crypt   *crypter // nil = env stored plaintext; set via SetSecretKey

	// vaultCrypt encrypts vault_secrets only, under a key nodes never receive
	// so a node holding the DSN cannot read tenant provider credentials. nil
	// falls back to crypt.
	vaultCrypt *crypter

	// allowPlaintextVault lets PutSecret store a credential unencrypted (dev only).
	allowPlaintextVault bool

	// firstTouch memoizes agent tokens whose first last_used write already happened.
	firstTouchMu sync.Mutex
	firstTouch   map[string]struct{}
}

// SetSecretKey enables at-rest encryption of stored env secrets with a 32-byte
// hex key (SANDBOXD_SECRET_KEY). Empty leaves encryption off; plaintext rows still read back.
func (s *Store) SetSecretKey(keyHex string) error {
	c, err := newCrypter(keyHex)
	if err != nil {
		return err
	}
	s.crypt = c
	return nil
}

// SetVaultKey sets a separate 32-byte hex key for vault_secrets (VAULT_SECRET_KEY).
// Control plane only; never set it on a node.
func (s *Store) SetVaultKey(keyHex string) error {
	c, err := newCrypter(keyHex)
	if err != nil {
		return err
	}
	s.vaultCrypt = c
	return nil
}

// SetAllowPlaintextVault permits storing vault secrets unencrypted. Local development only.
func (s *Store) SetAllowPlaintextVault(v bool) { s.allowPlaintextVault = v }

var errPlaintextVault = errors.New(
	"refusing to store a vault secret unencrypted: set VAULT_SECRET_KEY (64 hex chars) on the control plane " +
		"— it holds provider API keys, and without it they are readable by anything with the database DSN")

// vaultSeal encrypts with the vault key, falling back to the general crypter.
// Unlike sandbox env, a missing key is an error rather than plaintext passthrough.
func (s *Store) vaultSeal(v string) (string, error) {
	if s.vaultCrypt != nil {
		return s.vaultCrypt.seal(v)
	}
	if s.crypt == nil && !s.allowPlaintextVault {
		return "", errPlaintextVault
	}
	return s.crypt.seal(v)
}

// vaultOpen decrypts with the vault key, falling back to the general crypter
// for rows sealed before the vault key existed.
func (s *Store) vaultOpen(enc string) (string, error) {
	if s.vaultCrypt != nil {
		if plain, err := s.vaultCrypt.open(enc); err == nil {
			return plain, nil
		}
	}
	return s.crypt.open(enc)
}

// Open opens and migrates the store. A postgres:// or postgresql:// DSN selects
// Postgres; anything else is a SQLite file path. ctx bounds connect and migrate.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return openPostgres(ctx, dsn)
	}
	return openSQLite(ctx, dsn)
}

func openSQLite(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragma: %w", err)
	}
	s := &Store{db: db, dialect: dialectSQLite}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func openPostgres(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// Pool size caps concurrent requests, since nearly every request touches the DB.
	maxConns := envIntStore("DB_MAX_OPEN_CONNS", 20)
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns) // keep the pool warm
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(10 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{db: db, dialect: dialectPostgres}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// envIntStore reads a positive int from the environment, else def. Zero is
// rejected because database/sql treats it as unlimited.
func envIntStore(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("ignoring an invalid pool size; using the default",
			"component", "store", "var", name, "value", v, "default", def)
		return def
	}
	return n
}
