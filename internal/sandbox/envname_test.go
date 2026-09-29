package sandbox

import (
	"strings"
	"testing"
)

func TestValidEnvName(t *testing.T) {
	for _, ok := range []string{"A", "lower_case", "_x", "HELLO", "PDX", "PILLDROPSY"} {
		if err := ValidEnvName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1BAD", "A-B", "A B", "PD_SANDBOX_ID", "PILLDROPS_AGENT_MODE", strings.Repeat("A", 65)} {
		if ValidEnvName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidEnvName("PD_X"); err == nil || !strings.Contains(err.Error(), "PD_") {
		t.Errorf("reserved-prefix error must name the prefix: %v", err)
	}
}

// PD_DESKTOP is the one reserved name a caller may set, because it is how a
// caller asks for a screen and there is no other way to ask. The rest of the
// prefix must stay shut, or the hole is a second prefix rather than a name.
func TestPDDesktopIsCallerSettable(t *testing.T) {
	if err := ValidEnvName("PD_DESKTOP"); err != nil {
		t.Errorf("PD_DESKTOP refused (%v); nothing else can ask envd for a desktop", err)
	}
	for _, name := range []string{
		"PD_DESKTOP_PORT", // would move the viewer off the port the proxy dials
		"PD_SANDBOX_ID",   // the platform telling the guest who it is
		"PD_PREVIEW_BASE",
		"PD_DESKTOPX",
		"PD_",
		"PILLDROPS_AGENT_MODE",
	} {
		if err := ValidEnvName(name); err == nil {
			t.Errorf("%s accepted; the reserved prefix must stay shut apart from "+
				"the names opened by name", name)
		}
	}
}
