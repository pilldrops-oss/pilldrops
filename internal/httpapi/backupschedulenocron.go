//go:build core

package httpapi

import (
	"context"
	"errors"
)

// A core build ships no scheduler, so it cannot fire a backup job. Refusing
// the request is the honest answer: storing a schedule this binary will never
// run would read to an operator as backups being configured when they are not.
// On-demand backups and restore are unaffected — they need no sweeper.

func (s *Server) applyBackupSchedule(_ context.Context, _, _ string, _ BackupSchedule) error {
	return errors.New("this build has no scheduler, so backups cannot be scheduled; " +
		"POST /v1/sandboxes/{id}/backups still captures one on demand")
}

func (s *Server) backupScheduleView(_ context.Context, _, _ string) map[string]any { return nil }
