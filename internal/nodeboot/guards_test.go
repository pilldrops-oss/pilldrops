package nodeboot

import (
	"strings"
	"testing"
)

func TestIsolationGuard(t *testing.T) {
	cases := []struct {
		name        string
		devBuild    bool
		devInsecure bool
		isolated    bool
		wantErr     bool
	}{
		{"dev build, unset + no isolation → refuse (H10 fix)", true, false, false, true},
		{"dev build, unset + isolation → ok", true, false, true, false},
		{"dev build, devInsecure + no isolation → ok (explicit opt-out)", true, true, false, false},
		{"dev build, devInsecure + isolation → ok", true, true, true, false},

		{"release build, no isolation → refuse", false, false, false, true},
		{"release build, SANDBOXD_DEV cannot lower it", false, true, false, true},
		{"release build, isolation → ok", false, false, true, false},
		{"release build, isolation + devInsecure → ok", false, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := IsolationGuardError(tc.devBuild, tc.devInsecure, tc.isolated)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}

	// The release refusal must not point an operator at SANDBOXD_DEV, which does nothing there.
	err := IsolationGuardError(false, true, false)
	if err == nil {
		t.Fatal("release build with no isolation must refuse")
	}
	if !strings.Contains(err.Error(), "-tags dev") {
		t.Errorf("release refusal must name the build tag that would change the answer; got: %v", err)
	}
	if !strings.Contains(err.Error(), "will NOT start it") {
		t.Errorf("release refusal must say SANDBOXD_DEV does not apply, since the dev message does the opposite; got: %v", err)
	}
}

func TestProductionDBGuard(t *testing.T) {
	cases := []struct {
		name       string
		production bool
		dsn        string
		wantErr    bool
	}{
		{"prod + sqlite → refuse", true, "/var/lib/sandbox/state.db", true},
		{"prod + postgres:// → ok", true, "postgres://u:p@h/db", false},
		{"prod + postgresql:// → ok", true, "postgresql://u:p@h/db", false},
		{"dev + sqlite → ok", false, "/tmp/state.db", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ProductionDBError(tc.production, tc.dsn)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
