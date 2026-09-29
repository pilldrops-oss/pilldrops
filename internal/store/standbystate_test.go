package store

import "testing"

// standby is a state the daemon writes for a sandbox frozen in RAM. Migration
// 48's frozen value list predates that tier, so without a migration adding it
// the constraint refuses every standby write and the daemon cannot persist a
// sandbox it has just frozen.
func TestStandbyStateIsWritable(t *testing.T) {
	s := openTemp(t)
	err := s.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-standby", Template: "base", OrgID: "o1",
		State: "standby", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatalf("a sandbox frozen into standby cannot persist its own state: %v", err)
	}
}

// The set stays closed: adding standby must not turn the constraint into a
// formality that accepts anything.
func TestAnUnknownStateIsStillRefused(t *testing.T) {
	s := openTemp(t)
	err := s.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-bogus", Template: "base", OrgID: "o1",
		State: "hibernating", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1,
	})
	if err == nil {
		t.Fatal("an unknown state was accepted; the value list is no longer closed")
	}
}
