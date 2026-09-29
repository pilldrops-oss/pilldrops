package store

import "database/sql"

// PoolStats exposes the connection pool's state. WaitCount and WaitDuration
// climbing is the signature of a node waiting on the database.
func (s *Store) PoolStats() sql.DBStats { return s.db.Stats() }
