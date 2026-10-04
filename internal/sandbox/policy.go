package sandbox

import (
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// defaultLeaseSeconds is the lifetime an unset timeout receives; short and
// renewable via /refresh so an abandoned sandbox self-expires quickly.
const defaultLeaseSeconds = 900 // 15 min

// Resource floors; a request for less is raised, not honoured.
const (
	minVCPU      = 1
	minMemMB     = 128
	minStorageMB = 512
)

// Service defaults for an image-backed template, which has no catalogue entry
// and would otherwise clamp to the 128 MiB floor and OOM during init.
const (
	serviceDefaultMemMB = 1024
	serviceDefaultVCPU  = 2
)

// Validate checks everything decidable without a plan, a catalogue or a fleet,
// so cheap refusals happen before anything is reserved or booted. The
// validators are passed in so this package does not depend on the registry.
func (s Spec) Validate(knownTemplate func(string) bool, nameOK func(string) (string, error), repoOK func(url, ref string) error) (name string, err error) {
	// An image-backed create names its exact image, so it need not be in the catalogue.
	// A snapshot-backed one is the same case: the control plane has already
	// resolved the name to the id its capture was published under, and which
	// tier holds that capture — a rootfs on a Firecracker node, a committed
	// image on a container one — is the runtime's business, not the catalogue's.
	if s.Image == "" && s.Snapshot == "" && !knownTemplate(s.Template) {
		return "", invalid("unknown template: %s", s.Template)
	}
	if nameOK != nil {
		if name, err = nameOK(s.Name); err != nil {
			return "", &InvalidError{Reason: err.Error()}
		}
	}
	if err := ValidSlug(s.Slug); err != nil {
		return "", &InvalidError{Reason: err.Error()}
	}
	if err := ValidNetworks(s.Networks); err != nil {
		return "", &InvalidError{Reason: err.Error()}
	}
	if _, err := ValidVisibility(s.Visibility); err != nil {
		return "", &InvalidError{Reason: err.Error()}
	}
	if s.Repo != "" && repoOK != nil {
		if err := repoOK(s.Repo, s.RepoRef); err != nil {
			return "", &InvalidError{Reason: err.Error()}
		}
	}
	return name, nil
}

// Fleet is what Resolve needs to know about the current state of the world.
type Fleet struct {
	// PersistentCount is how many persistent sandboxes the org already holds.
	PersistentCount int
}

// Resolve applies plan policy to a validated request: request values, then the
// template recommendation, then clamped to the plan.
func Resolve(s Spec, tmpl TemplateDefaults, plan Limits, fleet Fleet, orgEgress *egress.Rules) (Resolved, error) {
	out := Resolved{Name: s.Name, Env: s.Env}

	out.TemplateID = s.Template
	if tmpl.ID != "" {
		out.TemplateID = tmpl.ID
	}

	out.VCPU, out.MemMiB, out.StorageMB = resources(s, tmpl, plan)

	ttl, err := lifetime(s, plan, fleet)
	if err != nil {
		return Resolved{}, err
	}
	out.TTL = ttl
	out.IdleTimeout = IdleTimeout(s.IdleTimeout)
	out.HibernateTimeout = HibernateTimeout(s.HibernateTimeout)
	out.Port = AppPort(s, tmpl)

	net, advisories, err := egressPolicy(s, tmpl, orgEgress)
	if err != nil {
		return Resolved{}, err
	}
	out.Network, out.NetworkAdvisories = net, advisories
	out.Networks = s.Networks
	return out, nil
}

// egressPolicy resolves the egress rules. An org override replaces the template
// default rather than composing with it; the request may only narrow. Validate
// after narrowing so a caller is not rejected for an entry the intersection discards.
func egressPolicy(s Spec, tmpl TemplateDefaults, orgEgress *egress.Rules) (*egress.Rules, []string, error) {
	base := tmpl.Network
	if orgEgress != nil {
		base = orgEgress
	}
	out := egress.Narrow(base, s.Network)
	if err := out.Validate(); err != nil {
		return nil, nil, invalid("network: %s", err)
	}
	return out, out.Advisories(), nil
}

// resources picks and clamps vCPU, memory and disk.
func resources(s Spec, tmpl TemplateDefaults, plan Limits) (vcpu, memMB, storageMB int) {
	vcpu, memMB, storageMB = s.VCPU, s.MemoryMB, s.StorageMB
	if vcpu <= 0 {
		vcpu = tmpl.CPU
	}
	if memMB <= 0 {
		memMB = tmpl.MemoryMB
	}
	if storageMB <= 0 {
		storageMB = tmpl.StorageMB
	}
	if s.Image != "" {
		if memMB <= 0 {
			memMB = serviceDefaultMemMB
		}
		if vcpu <= 0 {
			vcpu = serviceDefaultVCPU
		}
	}
	return clamp(vcpu, minVCPU, plan.MaxVCPU),
		clamp(memMB, minMemMB, plan.MaxMemMB),
		clamp(storageMB, minStorageMB, plan.MaxStorageMB)
}

// lifetime decides the hard TTL.
func lifetime(s Spec, plan Limits, fleet Fleet) (time.Duration, error) {
	ttl := time.Duration(s.Timeout) * time.Second
	switch {
	case s.Timeout < 0 && plan.Persistent:
		if plan.MaxWorkstations > 0 && fleet.PersistentCount >= plan.MaxWorkstations {
			return 0, limited(0, "plan '%s' includes %d persistent workstation(s) — delete one or upgrade",
				plan.ID, plan.MaxWorkstations)
		}
		return -1, nil
	case s.Timeout <= 0:
		// A negative timeout on a non-persistent plan degrades to the lease.
		ttl = defaultLeaseSeconds * time.Second
	}
	if planTTL := time.Duration(plan.SessionTTLHrs) * time.Hour; planTTL > 0 && ttl > planTTL {
		ttl = planTTL
	}
	return ttl, nil
}

// idleAutoPauseFloor keeps a per-sandbox idle override from thrashing.
const idleAutoPauseFloor = 30 * time.Second

// idleAutoPauseCeiling is a day; past that the abandonment sweep is the relevant bound.
const idleAutoPauseCeiling = 24 * time.Hour

// IdleTimeout maps a requested idle_timeout in seconds to the daemon's value:
// negative = never pause (-1), 0 = platform default, else clamped to the floor
// and ceiling. Shared by create and PATCH so both apply the same policy.
func IdleTimeout(secs int) time.Duration {
	switch {
	case secs < 0:
		return -1 // never pause
	case secs > 0:
		d := time.Duration(secs) * time.Second
		return clampDuration(d, idleAutoPauseFloor, idleAutoPauseCeiling)
	default:
		return 0 // platform default
	}
}

// defaultHibernateSeconds is boxd's four hours: long enough that a workstation
// you come back to the same day is still where you left it, short enough that
// an abandoned one stops holding RAM overnight.
const defaultHibernateSeconds = 4 * 60 * 60

// HibernateTimeout maps a requested hibernate_timeout in seconds to the
// daemon's value: negative = never (-1), 0 = the platform default, else clamped
// to the same floor and ceiling as the idle timer. Unlike IdleTimeout, 0 is
// resolved here rather than deferred to the node, so a sandbox created with no
// opinion reads back the number it will actually be hibernated at.
func HibernateTimeout(secs int) time.Duration {
	switch {
	case secs < 0:
		return -1
	case secs > 0:
		return clampDuration(time.Duration(secs)*time.Second, idleAutoPauseFloor, idleAutoPauseCeiling)
	default:
		return defaultHibernateSeconds * time.Second
	}
}

// IdleTimeoutSeconds is the inverse for responses: -1 stays -1, else whole seconds.
func IdleTimeoutSeconds(d time.Duration) int64 {
	if d < 0 {
		return -1
	}
	return int64(d / time.Second)
}

// defaultStartPort is the port a deploy publishes when the caller names none.
const defaultStartPort = 8000

// AppPort is the port a create publishes as the sandbox's app port: the
// requested one, else the deploy default when it runs a start command, else
// the template's own (0 when it has none).
func AppPort(s Spec, tmpl TemplateDefaults) int {
	if s.Port > 0 {
		return s.Port
	}
	if s.Start != "" {
		// The platform injects $PORT and the app is expected to bind it.
		return defaultStartPort
	}
	// A template with a built-in server declares its port; otherwise nothing is published.
	return tmpl.Port
}

// clamp bounds v to [lo, hi]; a non-positive hi means no ceiling.
func clamp(v, lo, hi int) int {
	if v < lo {
		v = lo
	}
	if hi > 0 && v > hi {
		v = hi
	}
	return v
}

func clampDuration(v, lo, hi time.Duration) time.Duration {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
