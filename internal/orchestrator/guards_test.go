package orchestrator

import (
	"strings"
	"testing"
)

func TestNodeKeySecretGuardError(t *testing.T) {
	cases := []struct {
		name        string
		clustered   bool
		devInsecure bool
		secret      string
		wantErr     bool
	}{
		{"clustered without the secret refuses", true, false, "", true},
		{"clustered with the secret starts", true, false, "s3cret", false},
		{"not clustered is unaffected", false, false, "", false},
		{"dev opt-in starts", true, true, "", false},
		{"dev opt-in with the secret still starts", true, true, "s3cret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NodeKeySecretGuardError(tc.clustered, tc.devInsecure, tc.secret)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NodeKeySecretGuardError(%v,%v,%q) = %v, wantErr=%v",
					tc.clustered, tc.devInsecure, tc.secret, err, tc.wantErr)
			}
		})
	}
}

// The operator sees only this string, so it must name the setting and the fix.
func TestNodeKeySecretGuardErrorNamesTheSettingAndTheFix(t *testing.T) {
	err := NodeKeySecretGuardError(true, false, "")
	if err == nil {
		t.Fatal("no error for a clustered control plane with no NODE_KEY_SECRET")
	}
	for _, want := range []string{
		"NODE_KEY_SECRET",
		"openssl rand",
		"re-join",
		"ORCHESTRATOR_TOKEN",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %s", want, err.Error())
		}
	}
}

// A handler test proves the door is shut; this proves the key behind it changed.
func TestNodeKeyDerivationHasNoFallbackToTheSharedToken(t *testing.T) {
	const fleetToken = "shared-orchestrator-token"

	// A node holds ORCHESTRATOR_TOKEN and could run the derivation with it.
	attacker := &Server{nodeKeySecret: fleetToken}

	s := NewServer("k", fleetToken)
	s.nodeKeySecret = "control-plane-only"

	for _, id := range []string{"node-a", "node-b", "node-c"} {
		if got, forged := s.nodeKeyFor(id, "par1"), attacker.nodeKeyFor(id, "par1"); got == forged {
			t.Errorf("%s: NODE_API_KEY is still derivable from ORCHESTRATOR_TOKEN", id)
		}
		if got, forged := s.nodeHeartbeatToken(id, "par1"), attacker.nodeHeartbeatToken(id, "par1"); got == forged {
			t.Errorf("%s: heartbeat token is still derivable from ORCHESTRATOR_TOKEN", id)
		}
	}

	// With NODE_KEY_SECRET unset the derivation must not quietly become the old one.
	unset := NewServer("k", fleetToken)
	if unset.nodeKeyFor("node-a", "par1") == attacker.nodeKeyFor("node-a", "par1") {
		t.Error("with NODE_KEY_SECRET unset the derivation fell back to ORCHESTRATOR_TOKEN")
	}
}

func TestOrgAssertKeyGuard(t *testing.T) {
	for _, tc := range []struct {
		name           string
		clustered, dev bool
		key            string
		wantRefusal    bool
	}{
		{"clustered, no key", true, false, "", true},
		{"clustered, key set", true, false, "abc123", false},
		{"clustered, dev-insecure", true, true, "", false},
		{"standalone, no key", false, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := OrgAssertKeyGuardError(tc.clustered, tc.dev, tc.key)
			if tc.wantRefusal && err == nil {
				t.Fatal("a clustered control plane with no ORG_ASSERT_KEY was allowed to start — " +
					"every X-Org-Override on this fleet will 403 with no indication why")
			}
			if !tc.wantRefusal && err != nil {
				t.Fatalf("refused a configuration that works: %v", err)
			}
			if tc.wantRefusal && !strings.Contains(err.Error(), "ORG_ASSERT_KEY") {
				t.Errorf("the refusal must name the setting; got %q", err)
			}
		})
	}
}
