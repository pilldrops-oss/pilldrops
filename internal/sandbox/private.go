package sandbox

import (
	"fmt"
	"regexp"
)

// PrivateTarget is one target a sandbox was granted, as the node routes it.
// Host is set only when the connector announced a hostname; IP hosts stay on
// the connector's machine.
type PrivateTarget struct {
	Name string `json:"name"`
	Host string `json:"host,omitempty"`
	Port int    `json:"port"`
}

const MaxPrivateTargets = 50

var privateName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidPrivateName(s string) bool { return privateName.MatchString(s) }

func ValidPrivateTargets(ts []PrivateTarget) error {
	if len(ts) > MaxPrivateTargets {
		return fmt.Errorf("at most %d private targets", MaxPrivateTargets)
	}
	seen := map[string]bool{}
	for _, t := range ts {
		if !ValidPrivateName(t.Name) {
			return fmt.Errorf("private target %q: name must be a lowercase DNS label", t.Name)
		}
		if t.Port < 1 || t.Port > 65535 {
			return fmt.Errorf("private target %q: port must be 1 to 65535", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("private target %q listed twice", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}
