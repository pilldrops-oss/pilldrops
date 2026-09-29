package store

import (
	"path/filepath"
	"testing"
	"time"
)

func approvalStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestApprovalRoundTrip(t *testing.T) {
	st := approvalStore(t)
	now := time.Now().Truncate(time.Second).UTC()

	in := AgentApproval{
		ID: "apr_1", SandboxID: "sb1", OrgID: "org1", TurnID: "turn_1",
		Tool: "Write", ToolUseID: "toolu_1", Signature: "Write: /work/*",
		ArgsDigest: "abc123", ArgsPreview: `{"file_path":"/work/x"}`,
		RequestedAt: now, State: ApprovalPending,
	}
	if err := st.UpsertAgentApproval(t.Context(), in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := st.ListAgentApprovals(t.Context(), "org1", "sb1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Tool != "Write" || got[0].Signature != "Write: /work/*" {
		t.Fatalf("row did not round trip: %+v", got[0])
	}
	if !got[0].RequestedAt.Equal(now) {
		t.Errorf("requested_at = %v, want %v", got[0].RequestedAt, now)
	}
	if got[0].Decided() {
		t.Error("a pending approval reports itself as decided")
	}
	if !got[0].DecidedAt.IsZero() {
		t.Errorf("decided_at = %v on a pending approval", got[0].DecidedAt)
	}
}

func TestADecisionIsNeverOverwrittenByALaterPoll(t *testing.T) {
	st := approvalStore(t)
	base := AgentApproval{
		ID: "apr_1", SandboxID: "sb1", OrgID: "org1", Tool: "Bash",
		RequestedAt: time.Now(), State: ApprovalPending,
	}
	if err := st.UpsertAgentApproval(t.Context(), base); err != nil {
		t.Fatal(err)
	}

	decided := base
	decided.State, decided.DecidedBy, decided.DecidedAt = ApprovalAllowed, "key:alice", time.Now()
	if err := st.UpsertAgentApproval(t.Context(), decided); err != nil {
		t.Fatal(err)
	}

	if err := st.UpsertAgentApproval(t.Context(), base); err != nil {
		t.Fatal(err)
	}

	got, _ := st.ListAgentApprovals(t.Context(), "org1", "sb1", 10)
	if got[0].State != ApprovalAllowed {
		t.Fatalf("state = %s after a stale poll, want allowed — the trail now understates "+
			"what happened", got[0].State)
	}
	if got[0].DecidedBy != "key:alice" {
		t.Errorf("decided_by lost: %q", got[0].DecidedBy)
	}
}

func TestOneOutcomeIsNeverReplacedByAnother(t *testing.T) {
	st := approvalStore(t)
	a := AgentApproval{ID: "apr_1", SandboxID: "sb1", OrgID: "org1",
		RequestedAt: time.Now(), State: ApprovalDenied, DecidedBy: "key:alice"}
	if err := st.UpsertAgentApproval(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	a.State, a.DecidedBy = ApprovalAllowed, "key:bob"
	if err := st.UpsertAgentApproval(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListAgentApprovals(t.Context(), "org1", "sb1", 10)
	if got[0].State != ApprovalDenied {
		t.Fatalf("a denial was overwritten by an allow: %s", got[0].State)
	}
}

func TestALaterWriteFillsMissingDescription(t *testing.T) {
	st := approvalStore(t)
	if err := st.UpsertAgentApproval(t.Context(), AgentApproval{
		ID: "apr_1", SandboxID: "sb1", OrgID: "org1",
		RequestedAt: time.Now(), State: ApprovalAllowed, DecidedBy: "key:alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAgentApproval(t.Context(), AgentApproval{
		ID: "apr_1", SandboxID: "sb1", OrgID: "org1",
		Tool: "Write", Signature: "Write: /work/*", ArgsPreview: `{"file_path":"/work/x"}`,
		State: ApprovalPending,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListAgentApprovals(t.Context(), "org1", "sb1", 10)
	if got[0].State != ApprovalAllowed {
		t.Fatalf("the outcome was lost while filling description: %s", got[0].State)
	}
	if got[0].Signature != "Write: /work/*" || got[0].Tool != "Write" {
		t.Fatalf("description was not filled in: %+v", got[0])
	}
}

func TestApprovalsAreOrgScoped(t *testing.T) {
	st := approvalStore(t)
	if err := st.UpsertAgentApproval(t.Context(), AgentApproval{
		ID: "apr_1", SandboxID: "sb1", OrgID: "org1",
		RequestedAt: time.Now(), State: ApprovalPending,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListAgentApprovals(t.Context(), "org2", "sb1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("another org read %d approvals for a sandbox it does not own", len(got))
	}
}

func TestTimeoutIsDistinctFromDenial(t *testing.T) {
	st := approvalStore(t)
	for i, s := range []ApprovalState{ApprovalDenied, ApprovalTimedOut} {
		if err := st.UpsertAgentApproval(t.Context(), AgentApproval{
			ID: string(rune('a' + i)), SandboxID: "sb1", OrgID: "org1",
			RequestedAt: time.Now(), State: s,
		}); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := st.CountAgentApprovals(t.Context(), "org1", "sb1")
	if err != nil {
		t.Fatal(err)
	}
	if counts[ApprovalDenied] != 1 || counts[ApprovalTimedOut] != 1 {
		t.Fatalf("timeout and denial were collapsed: %v", counts)
	}
}

func TestUpsertRejectsBadInput(t *testing.T) {
	st := approvalStore(t)
	for _, a := range []AgentApproval{
		{SandboxID: "sb1"}, // no id
		{ID: "apr_1"},      // no sandbox
		{ID: "apr_1", SandboxID: "sb1", State: "whatever"}, // unknown state
	} {
		if err := st.UpsertAgentApproval(t.Context(), a); err == nil {
			t.Errorf("accepted %+v", a)
		}
	}
	// An empty state defaults to pending.
	if err := st.UpsertAgentApproval(t.Context(), AgentApproval{ID: "apr_ok", SandboxID: "sb1", OrgID: "o"}); err != nil {
		t.Errorf("an unset state was rejected: %v", err)
	}
}
