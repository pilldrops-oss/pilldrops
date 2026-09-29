package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// The backup schedule is one hidden cron job per sandbox, named backupJobName.
// A schedule rather than a column because the sweeper, the claim that stops two
// replicas firing the same job, and the run history already exist there.

// backupJobName is the reserved name of a sandbox's backup job.
const backupJobName = "backup"

// BackupFloor is the shortest allowed period between backups. A capture pauses
// the machine and writes a disk image; below this it costs more than it saves.
const BackupFloor = 15 * time.Minute

// BackupSchedule is the request shape of {"backups": {...}} on the config route.
type BackupSchedule struct {
	// Period is a duration like "24h". Mutually exclusive with Cron.
	Period string `json:"period"`
	// Cron is a 5-field expression.
	Cron string `json:"cron"`
	// Retention is how long this sandbox's backups are kept, e.g. "7d".
	Retention string `json:"retention"`
}

// ErrScheduleTooFrequent names the floor rather than restating the input.
var ErrScheduleTooFrequent = fmt.Errorf("a backup schedule must be at least %s apart", BackupFloor)

// cronForPeriod turns a period into an expression the sweeper understands.
// Only whole hours and whole minutes: a 5-field expression cannot express
// "every 90 minutes", and pretending otherwise would silently round.
func cronForPeriod(period string) (string, error) {
	d, err := time.ParseDuration(period)
	if err != nil {
		return "", fmt.Errorf("period %q is not a duration like 24h or 30m", period)
	}
	switch {
	case d < BackupFloor:
		return "", ErrScheduleTooFrequent
	case d%time.Hour == 0 && d <= 24*time.Hour:
		if d == 24*time.Hour {
			return "0 0 * * *", nil
		}
		return fmt.Sprintf("0 */%d * * *", int(d/time.Hour)), nil
	case d%time.Minute == 0 && d < time.Hour:
		return fmt.Sprintf("*/%d * * * *", int(d/time.Minute)), nil
	default:
		return "", fmt.Errorf("period %q must be a whole number of minutes under an hour, "+
			"or a whole number of hours up to 24h", period)
	}
}

// parseRetention reads "7d", "48h" or "" (the deployment default).
func parseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// "7d" is not a Go duration; hours are, so days go through hours and the
	// positivity check below covers both spellings rather than only one.
	if days, ok := strings.CutSuffix(s, "d"); ok {
		hours, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, fmt.Errorf("retention %q is not a duration like 7d or 48h", s)
		}
		s = (hours * 24).String()
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("retention %q is not a positive duration like 7d or 48h", s)
	}
	return d, nil
}

// backupScheduleErr maps a schedule refusal to a status code. All but one are
// the caller's input, so they are 400s naming the rule; a node that cannot back
// up at all answers exactly as the backup verbs do, so a caller learns the same
// thing from `backup schedule` as from `backup create`.
func backupScheduleErr(w http.ResponseWriter, err error) {
	if errors.Is(err, daemon.ErrBackupsUnconfigured) {
		writeErrCode(w, http.StatusNotImplemented, "backups_need_object_store", err.Error())
		return
	}
	writeErrCode(w, http.StatusBadRequest, "invalid_backup_schedule", err.Error())
}
