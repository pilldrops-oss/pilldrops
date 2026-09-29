package agentd

import "testing"

func TestDefaultAutonomyFallsBackWhenTheChoiceIsUnavailable(t *testing.T) {
	// No GuidedAvailable func, so guided is not offered.
	s := &Supervisor{DefaultAutonomy: ModeGuided}
	got := s.autonomy()

	for _, m := range got.Available {
		if m == ModeGuided {
			t.Fatal("guided is offered with no GuidedAvailable func — the ladder is wrong")
		}
	}
	if got.Default == ModeGuided {
		t.Error("reports guided as the default while guided is not available: turns would " +
			"run ungated while status claims they are reviewed")
	}
	if got.Default != ModeAutonomous {
		t.Errorf("default = %q, want the autonomous fallback", got.Default)
	}
	if got.GuidedUnavailableReason == "" {
		t.Error("guided is missing with no reason, which reads as a bug rather than a limit")
	}
}

func TestDefaultAutonomyIsHonouredWhenAvailable(t *testing.T) {
	s := &Supervisor{DefaultAutonomy: ModeGuided, GuidedAvailable: func() bool { return true }}
	if got := s.autonomy(); got.Default != ModeGuided {
		t.Errorf("default = %q, want guided — the launch choice was discarded", got.Default)
	}
}

func TestDefaultAutonomyUnsetKeepsTheHistoricalBehaviour(t *testing.T) {
	// Guided as the implicit default would start parking writes for every existing API caller.
	s := &Supervisor{GuidedAvailable: func() bool { return true }}
	if got := s.autonomy(); got.Default != ModeAutonomous {
		t.Errorf("default = %q with no launch choice, want autonomous", got.Default)
	}
}

func TestDefaultAutonomySupervisedIsHonoured(t *testing.T) {
	s := &Supervisor{DefaultAutonomy: ModeSupervised}
	if got := s.autonomy(); got.Default != ModeSupervised {
		t.Errorf("default = %q, want supervised", got.Default)
	}
}

func TestDefaultAutonomyIgnoresNonsense(t *testing.T) {
	// The value comes from an env var, so a typo must not become a mode.
	s := &Supervisor{DefaultAutonomy: "yolo", GuidedAvailable: func() bool { return true }}
	if got := s.autonomy(); got.Default != ModeAutonomous {
		t.Errorf("default = %q for an unknown mode, want the autonomous fallback", got.Default)
	}
}
