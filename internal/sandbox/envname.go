package sandbox

import (
	"fmt"
	"regexp"
	"strings"
)

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// reservedEnvPrefixes are set by the platform itself (PILLDROPS_AGENT_MODE,
// PD_SANDBOX_ID, …); a caller must not be able to shadow them.
var reservedEnvPrefixes = []string{"PILLDROPS_", "PD_"}

// callerSettableEnv are the reserved-prefix names a caller MAY set, because
// they are an *input* to the platform rather than a fact the platform reports.
// PD_SANDBOX_ID is the platform telling the guest who it is, and a caller
// setting it would be lying; PD_DESKTOP is the caller telling envd to start a
// screen, and there is no other way to ask.
//
// Deliberately a list of exact names, not a second prefix: every hole in the
// reserved rule is named here or it does not exist. PD_DESKTOP_PORT is
// deliberately NOT in it — the preview proxy and the Docker publish both use
// the fixed platform port, so a caller moving the viewer elsewhere would only
// break their own preview.
var callerSettableEnv = map[string]bool{
	"PD_DESKTOP": true,
}

// ValidEnvName accepts a POSIX-style identifier of at most 64 characters that
// does not start with a reserved platform prefix, unless it is one of the few
// reserved names callerSettableEnv opens by name. Shared by create env, the
// config endpoint and the vault, so a name is legal everywhere or nowhere.
func ValidEnvName(name string) error {
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("invalid variable name %q: letters, digits and underscore only, not starting with a digit, max 64 characters", name)
	}
	if callerSettableEnv[name] {
		return nil
	}
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("variable name %q uses the reserved prefix %s, which the platform sets", name, p)
		}
	}
	return nil
}
