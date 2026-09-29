package main

import "strings"

// execMode is where sandbox work runs. The zero value execRefuse fails closed.
type execMode int

const (
	// execRefuse is the zero value so a half-configured deployment fails closed.
	execRefuse execMode = iota
	// execProxy forwards sandbox work to the orchestrator fleet.
	execProxy
	// execLocal runs sandbox work on this host; local dev only, and only when
	// explicitly asked for.
	execLocal
)

func (m execMode) String() string {
	switch m {
	case execProxy:
		return "fleet"
	case execLocal:
		return "local-host"
	default:
		return "refuse"
	}
}

// PortalLocalRuntimeEnv opts a process into the host runtime. `make dev` sets
// it; production must not.
const PortalLocalRuntimeEnv = "PORTAL_LOCAL_RUNTIME"

// sandboxExecMode picks fleet mode by presence of a complete config and host
// execution by presence of an explicit opt-in, never by absence of the other.
// A configured fleet always wins.
func sandboxExecMode(orchURL, orchKey, localOptIn string) execMode {
	if strings.TrimSpace(orchURL) != "" && strings.TrimSpace(orchKey) != "" {
		return execProxy
	}
	if truthyEnv(localOptIn) {
		return execLocal
	}
	return execRefuse
}

// truthyEnv accepts only a few spellings; an ambiguous value must not run
// customer code on this host.
func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
