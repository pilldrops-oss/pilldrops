package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustCreateBackup(t *testing.T, st *Store, org, sandbox string) Backup {
	t.Helper()
	b, err := st.CreateBackup(t.Context(), Backup{OrgID: org, SandboxID: sandbox, NodeID: "n1"})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	return b
}

func TestBackupRoundTrip(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	b := mustCreateBackup(t, st, "acme", "sb-1")
	if b.ID == "" || b.Status != BackupPending || b.CreatedAt == 0 {
		t.Fatalf("a fresh backup reads %+v; want a pending row with an id and a timestamp", b)
	}
	if _, ok := st.PendingBackup(t.Context(), "sb-1"); !ok {
		t.Fatal("a pending capture must be findable, or a second one is not refused")
	}

	if err := st.SetBackupReady(t.Context(), b.ID, "disk", "backups/acme/sb-1/"+b.ID, "abc123", "", 4096); err != nil {
		t.Fatal(err)
	}
	got, ok := st.BackupByID(t.Context(), "acme", b.ID)
	if !ok {
		t.Fatal("the row vanished")
	}
	if got.Status != BackupReady || got.Bytes != 4096 || got.SHA256 != "abc123" ||
		got.Kind != "disk" || got.ObjectKey == "" || got.ReadyAt == 0 {
		t.Fatalf("ready backup reads %+v", got)
	}
	if _, ok := st.PendingBackup(t.Context(), "sb-1"); ok {
		t.Error("a ready capture still reads as pending; a second backup would be refused forever")
	}
	if _, ok := st.BackupByID(t.Context(), "other-org", b.ID); ok {
		t.Error("another org can read this backup")
	}

	last, ok := st.LastReadyBackup(t.Context(), "sb-1")
	if !ok || last.ID != b.ID {
		t.Fatalf("LastReadyBackup = %+v, %v; want %s", last, ok, b.ID)
	}
}

func TestBackupFailureKeepsTheReason(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	b := mustCreateBackup(t, st, "acme", "sb-1")
	if err := st.SetBackupFailed(t.Context(), b.ID, "overlay copy: no space left on device"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.BackupByID(t.Context(), "acme", b.ID)
	if got.Status != BackupFailed || got.Error == "" {
		t.Fatalf("failed backup reads %+v; the owner needs to know why", got)
	}
	if _, ok := st.LastReadyBackup(t.Context(), "sb-1"); ok {
		t.Error("a failed capture must not be a dedupe base: its bytes are not in the store")
	}
}

// A deduped row has no object of its own; deleting its parent would strand it.
func TestDeleteRefusesAReferencedBackup(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	parent := mustCreateBackup(t, st, "acme", "sb-1")
	if err := st.SetBackupReady(t.Context(), parent.ID, "disk", "k1", "same", "", 100); err != nil {
		t.Fatal(err)
	}
	child := mustCreateBackup(t, st, "acme", "sb-1")
	if err := st.SetBackupReady(t.Context(), child.ID, "disk", "", "same", parent.ID, 100); err != nil {
		t.Fatal(err)
	}

	err := st.DeleteBackup(t.Context(), "acme", parent.ID)
	if !errors.Is(err, ErrBackupReferenced) {
		t.Fatalf("deleting a referenced backup = %v, want ErrBackupReferenced", err)
	}
	if !strings.Contains(err.Error(), child.ID) {
		t.Errorf("the refusal must name the child that depends on it, got %v", err)
	}

	// The leaf goes, and then the parent may.
	if err := st.DeleteBackup(t.Context(), "acme", child.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBackup(t.Context(), "acme", parent.ID); err != nil {
		t.Fatalf("with no children left, delete = %v", err)
	}
}

func TestDeleteBackupIsOrgScoped(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	b := mustCreateBackup(t, st, "acme", "sb-1")
	if err := st.DeleteBackup(t.Context(), "other", b.ID); err == nil {
		t.Fatal("another org deleted this backup")
	}
	if _, ok := st.BackupByID(t.Context(), "acme", b.ID); !ok {
		t.Fatal("the row was removed by the refused delete")
	}
}

func TestExpiredBackupsSkipsRowsWithChildren(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	now := time.Now().Unix()

	// Expired, no child: reclaimable.
	lone := mustCreateBackup(t, st, "acme", "sb-1")
	mustExpire(t, st, lone.ID, now-10)

	// Expired, but a later backup dedupes against it.
	parent := mustCreateBackup(t, st, "acme", "sb-2")
	mustExpire(t, st, parent.ID, now-10)
	child := mustCreateBackup(t, st, "acme", "sb-2")
	if err := st.SetBackupReady(t.Context(), child.ID, "disk", "", "same", parent.ID, 1); err != nil {
		t.Fatal(err)
	}

	// Not expired yet.
	fresh := mustCreateBackup(t, st, "acme", "sb-3")
	mustExpire(t, st, fresh.ID, now+3600)

	got, err := st.ExpiredBackups(t.Context(), now, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	if !ids[lone.ID] {
		t.Error("an expired leaf was not offered for reclaim")
	}
	if ids[parent.ID] {
		t.Error("an expired backup another backup restores from was offered for reclaim")
	}
	if ids[fresh.ID] {
		t.Error("a backup that has not expired was offered for reclaim")
	}
}

// expires_at 0 means "use the deployment default", and 0 days means keep forever.
func TestExpiredBackupsAppliesTheDefaultRetention(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	now := time.Now().Unix()

	old := mustCreateBackup(t, st, "acme", "sb-1")
	if _, err := st.exec(t.Context(), `UPDATE backups SET created_at = ? WHERE id = ?`, now-30*24*3600, old.ID); err != nil {
		t.Fatal(err)
	}

	got, err := st.ExpiredBackups(t.Context(), now, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != old.ID {
		t.Fatalf("a 30-day-old row under a 7-day default was not expired: %+v", got)
	}

	keepForever, err := st.ExpiredBackups(t.Context(), now, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keepForever) != 0 {
		t.Fatalf("a 0-day default must keep everything, got %+v", keepForever)
	}
}

func TestCronJobKindDefaultsToShell(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	j, err := st.CreateCronJob(t.Context(), CronJob{OrgID: "acme", SandboxID: "sb-1", Command: "echo hi"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.CronJobByID(t.Context(), "acme", j.ID)
	if got.Kind != CronKindShell {
		t.Fatalf("kind = %q, want %q — an existing job must keep running as a shell job", got.Kind, CronKindShell)
	}

	// A backup job needs no command, and the kind survives the round trip.
	bj, err := st.CreateCronJob(t.Context(), CronJob{OrgID: "acme", SandboxID: "sb-1", Kind: CronKindBackup}, 0)
	if err != nil {
		t.Fatalf("a backup job must not need a command: %v", err)
	}
	if got, _ := st.CronJobByID(t.Context(), "acme", bj.ID); got.Kind != CronKindBackup {
		t.Fatalf("kind = %q, want backup", got.Kind)
	}

	if _, err := st.CreateCronJob(t.Context(), CronJob{OrgID: "acme", SandboxID: "sb-1", Kind: "rm-rf", Command: "x"}, 0); err == nil {
		t.Error("an unknown kind was accepted; the sweeper would not know what to do with it")
	}
}

func mustExpire(t *testing.T, st *Store, id string, at int64) {
	t.Helper()
	if _, err := st.exec(t.Context(), `UPDATE backups SET expires_at = ? WHERE id = ?`, at, id); err != nil {
		t.Fatal(err)
	}
}
