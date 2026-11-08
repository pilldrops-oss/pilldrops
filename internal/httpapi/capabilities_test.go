package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/apispec"
	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// No build tag: /v1/capabilities is a core route, so these run in both builds.

type fakeRuntime struct {
	name string
	caps rt.Caps
	none bool
	idle time.Duration
	// hibernate is the disk-pause timer AutoPause now reads; 0 means the idle
	// value, so existing cases keep their meaning.
	hibernate time.Duration
}

func (f fakeRuntime) RuntimeName() string               { return f.name }
func (f fakeRuntime) IdleTimeoutDefault() time.Duration { return f.idle }
func (f fakeRuntime) HibernateTimeoutDefault() time.Duration {
	if f.hibernate != 0 {
		return f.hibernate
	}
	return f.idle
}
func (f fakeRuntime) RuntimeCaps() (rt.Caps, bool) {
	if f.none {
		return rt.Caps{}, false
	}
	return f.caps, true
}

// A node must describe the engine it boots, not a hardcoded one.
func TestNodeReportsExactlyItsOwnRuntime(t *testing.T) {
	for _, tc := range []struct {
		rd        fakeRuntime
		wantLevel rt.IsolationLevel
		wantSnap  bool
	}{
		{fakeRuntime{name: "firecracker", caps: rt.Caps{Isolation: true, MultiPort: true}}, rt.IsolationStrong, false},
		{fakeRuntime{name: "firecracker", caps: rt.Caps{Isolation: true, Snapshot: true, MultiPort: true}}, rt.IsolationStrong, true},
		{fakeRuntime{name: "docker", caps: rt.Caps{Isolation: true, Snapshot: true}}, rt.IsolationShared, true},
		{fakeRuntime{name: "process", caps: rt.Caps{Snapshot: true}}, rt.IsolationTrusted, true},
	} {
		t.Run(tc.rd.name+"/"+string(tc.wantLevel), func(t *testing.T) {
			got, ok := buildNodeCapabilities(tc.rd, nil, entitlements.Unlimited(), 0)
			if !ok {
				t.Fatal("buildNodeCapabilities reported no runtime")
			}
			if got.Runtime != tc.rd.name {
				t.Errorf("runtime = %q, want %q", got.Runtime, tc.rd.name)
			}
			if got.IsolationLevel != tc.wantLevel {
				t.Errorf("isolation_level = %q, want %q", got.IsolationLevel, tc.wantLevel)
			}
			if got.Isolation != tc.rd.caps.Isolation {
				t.Errorf("isolation = %v, want %v (the raw Caps bit must agree with what "+
					"the heartbeat sends)", got.Isolation, tc.rd.caps.Isolation)
			}
			if got.Snapshot != tc.wantSnap {
				t.Errorf("snapshot = %v, want %v", got.Snapshot, tc.wantSnap)
			}
			if got.MultiPort != tc.rd.caps.MultiPort {
				t.Errorf("multiport = %v, want %v", got.MultiPort, tc.rd.caps.MultiPort)
			}
			if got.AutoPause {
				t.Error("auto_pause = true with the idle timer off; idle sandboxes are never paused then")
			}
			if got.Scope != "node" {
				t.Errorf("scope = %q, want \"node\"", got.Scope)
			}
		})
	}
}

// An unrecognised isolating runtime gets the weaker level.
func TestUnknownRuntimeIsNeverReportedStrong(t *testing.T) {
	got, ok := buildNodeCapabilities(
		fakeRuntime{name: "gvisor", caps: rt.Caps{Isolation: true}}, nil, entitlements.Unlimited(), 0)
	if !ok {
		t.Fatal("no runtime")
	}
	if got.IsolationLevel != rt.IsolationShared {
		t.Errorf("isolation_level = %q for an unrecognised isolating runtime, want %q — "+
			"an unreviewed driver must not inherit the strongest claim this platform makes",
			got.IsolationLevel, rt.IsolationShared)
	}
}

// A zero Caps would serialise as isolation_level "trusted", so no runtime must refuse.
func TestNoRuntimeIsRefusedNotDescribed(t *testing.T) {
	if _, ok := buildNodeCapabilities(fakeRuntime{none: true}, nil, entitlements.Unlimited(), 0); ok {
		t.Fatal("a describer with no runtime produced a capabilities document; it must refuse, " +
			"because the document it would produce claims there is no tenant boundary")
	}
}

// nil (does not enumerate) and empty (holds nothing) both marshal to [] but mean opposite things.
func TestTemplatesEnumeratedDistinguishesColdFromUnrestricted(t *testing.T) {
	fc := fakeRuntime{name: "firecracker", caps: rt.Caps{Isolation: true}}

	unrestricted, _ := buildNodeCapabilities(fc, nil, entitlements.Unlimited(), 0)
	if unrestricted.TemplatesEnumerated {
		t.Error("templates_enumerated = true with no reporter installed — this node does not enumerate")
	}

	cold, _ := buildNodeCapabilities(fc, func() []string { return nil }, entitlements.Unlimited(), 0)
	if !cold.TemplatesEnumerated {
		t.Error("templates_enumerated = false for a node that enumerates and holds nothing — " +
			"that reads as \"boots anything\", the opposite of the truth")
	}
	if cold.Templates == nil {
		t.Error("templates = null, want []")
	}

	warm, _ := buildNodeCapabilities(fc, func() []string { return []string{"base"} }, entitlements.Unlimited(), 0)
	if !warm.TemplatesEnumerated || len(warm.Templates) != 1 || warm.Templates[0] != "base" {
		t.Errorf("templates = %v (enumerated %v), want [base] true", warm.Templates, warm.TemplatesEnumerated)
	}
}

func TestCapabilityLimitsComeFromThePlanAndTheNode(t *testing.T) {
	plan := entitlements.Plan{
		MaxVCPU: 4, MaxMemMB: 8192, MaxStorageMB: 20480,
		MaxConcurrent: 10, SessionTTLHrs: 24,
	}
	got, ok := buildNodeCapabilities(
		fakeRuntime{name: "firecracker", caps: rt.Caps{Isolation: true}}, nil, plan, 95)
	if !ok {
		t.Fatal("no runtime")
	}
	if got.Limits.MaxVCPU != 4 || got.Limits.MaxMemMB != 8192 || got.Limits.MaxStorageMB != 20480 {
		t.Errorf("limits = %+v, want the plan's ceilings", got.Limits)
	}
	if got.Limits.MaxConcurrent != 10 || got.Limits.SessionTTLHrs != 24 {
		t.Errorf("limits = %+v, want the plan's concurrency and TTL", got.Limits)
	}
	if got.Limits.NodeMaxActive != 95 {
		t.Errorf("node_max_active = %d, want 95 — the node's own ceiling binds every org "+
			"together and is not in any plan", got.Limits.NodeMaxActive)
	}
}

// End to end through the real daemon and the process runtime the test server boots.
func TestCapabilitiesEndpoint(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "GET", srv.URL+"/v1/capabilities", key, "")
	if code != 200 {
		t.Fatalf("GET /v1/capabilities = %d, want 200: %v", code, out)
	}
	// Decode into the wire struct so the JSON tags are asserted too.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got nodeCapabilities
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if got.Scope != "node" {
		t.Errorf("scope = %q, want \"node\"", got.Scope)
	}
	if len(got.Features) != 1 || got.Features[0] != "volume" {
		t.Errorf("features = %#v, want [volume]: a node serves volumes and no cloud groups, and absent would read as a server too old to say", got.Features)
	}
	if got.Runtime != "process" {
		t.Errorf("runtime = %q, want \"process\" — the test server boots the process runtime, "+
			"and the endpoint must report the engine it HAS", got.Runtime)
	}
	if got.IsolationLevel != rt.IsolationTrusted {
		t.Errorf("isolation_level = %q, want %q for the process runtime",
			got.IsolationLevel, rt.IsolationTrusted)
	}
	if got.Isolation {
		t.Error("isolation = true for the process runtime, which has no boundary at all")
	}
	// The test daemon runs the process runtime (declares Snapshot) with a one-hour
	// idle timer, so the truthful answer is true; a node with the timer off says false.
	if !got.AutoPause {
		t.Error("auto_pause = false, want true: the test daemon snapshots and its idle timer is on")
	}
}

// nested_docker must be on the wire (not absent, which a client cannot tell
// from false) and must mirror the runtime's own bit both ways. Which adapter
// declares it is pinned by TestRuntimeCapabilitiesMatchTheDoc.
func TestNestedDockerIsReportedBothWays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nested bool
	}{
		{"firecracker", true},
		{"docker", false},
		{"process", false},
	} {
		rd := fakeRuntime{name: tc.name, caps: rt.Caps{Isolation: true, NestedDocker: tc.nested}}
		got, ok := buildNodeCapabilities(rd, nil, entitlements.Unlimited(), 0)
		if !ok {
			t.Fatal("no runtime")
		}
		if got.NestedDocker != tc.nested {
			t.Errorf("%s: nested_docker = %v, want %v", tc.name, got.NestedDocker, tc.nested)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		v, present := wire["nested_docker"]
		if !present {
			t.Fatalf("%s: nested_docker is absent from the JSON; a client cannot tell absent from false", tc.name)
		}
		if v != tc.nested {
			t.Errorf("%s: JSON nested_docker = %v, want %v", tc.name, v, tc.nested)
		}
	}
}

// auto_pause tells the truth: true only when pause frees RAM AND the idle timer is on.
func TestAutoPauseIsSnapshotAndIdleTimer(t *testing.T) {
	for _, tc := range []struct {
		snap bool
		idle time.Duration
		want bool
	}{
		{true, 5 * time.Minute, true},
		{true, 0, false},
		{true, -1, false},
		{false, 5 * time.Minute, false},
	} {
		rd := fakeRuntime{name: "firecracker", caps: rt.Caps{Isolation: true, Snapshot: tc.snap}, idle: tc.idle}
		got, ok := buildNodeCapabilities(rd, nil, entitlements.Unlimited(), 0)
		if !ok {
			t.Fatal("no runtime")
		}
		if got.AutoPause != tc.want {
			t.Errorf("snapshot=%v idle=%s: auto_pause = %v, want %v", tc.snap, tc.idle, got.AutoPause, tc.want)
		}
	}
}

// openapi.json is hand-maintained and the SDKs are generated from it.
func TestCapabilitiesIsInTheSpec(t *testing.T) {
	var spec struct {
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(apispec.JSON(), &spec); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}
	ops, ok := spec.Paths["/v1/capabilities"]
	if !ok {
		t.Fatal("openapi.json has no /v1/capabilities path — the generated SDKs and the " +
			"reference will not know the endpoint exists")
	}
	if _, ok := ops["get"]; !ok {
		t.Error("openapi.json documents /v1/capabilities without its GET operation")
	}
	// The path serves a different document per tier, so both schemas must exist.
	for _, s := range []string{"NodeCapabilities", "FleetCapabilities"} {
		if _, ok := spec.Components.Schemas[s]; !ok {
			t.Errorf("openapi.json has no %s schema; /v1/capabilities answers with a different "+
				"shape on each tier and both have to be described", s)
		}
	}
}

// Asserts the body, not only the status: an unmounted route on the core build
// also answers with a non-JSON error.
func TestCapabilitiesRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /v1/capabilities = %d, want 401", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the 401 body is not JSON (%v) — that is what an UNMOUNTED route looks "+
			"like on this mux, not a refused one", err)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("401 body = %v, want an \"error\" field from the auth middleware", body)
	}
}

// Reported even when 0, so "no warm path" is explicit rather than absent.
func TestCapabilitiesReportWarmRestoreStorageSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps rt.Caps
		want int
	}{
		{"snapshot-serving firecracker node", rt.Caps{Isolation: true, WarmRestoreStorageMB: 5120}, 5120},
		{"cold-boot-only node", rt.Caps{Isolation: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := buildNodeCapabilities(fakeRuntime{name: "firecracker", caps: tc.caps}, nil, entitlements.Unlimited(), 0)
			if !ok {
				t.Fatal("buildNodeCapabilities reported no runtime")
			}
			if got.WarmRestoreStorageMB != tc.want {
				t.Errorf("warm_restore_storage_mb = %d, want %d", got.WarmRestoreStorageMB, tc.want)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var onWire map[string]any
			if err := json.Unmarshal(b, &onWire); err != nil {
				t.Fatal(err)
			}
			if _, present := onWire["warm_restore_storage_mb"]; !present {
				t.Error("warm_restore_storage_mb missing from the JSON document")
			}
		})
	}
}

func TestWhoamiNamesTheKeyOnANode(t *testing.T) {
	srv, key := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code %d", resp.StatusCode)
	}
	var who map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	if who["principal"] == "" || who["org"] == "" {
		t.Errorf("whoami = %v, want a principal and an org", who)
	}

	anonymous, err := http.Get(srv.URL + "/v1/whoami")
	if err != nil {
		t.Fatal(err)
	}
	defer anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: code %d, want 401", anonymous.StatusCode)
	}
}
