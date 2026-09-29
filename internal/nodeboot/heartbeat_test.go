package nodeboot

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The advertised list is the disk scan, never the TEMPLATE= wish list.
func TestLocalTemplatesAdvertisesOnlyWhatIsOnDisk(t *testing.T) {
	t.Setenv("TEMPLATE", "base,unbuilt")

	report := LocalTemplates("firecracker", func() []string { return []string{"base"} })
	if report == nil {
		t.Fatal("a firecracker node reported nil, which the scheduler reads as UNRESTRICTED")
	}
	got := report()
	if !reflect.DeepEqual(got, []string{"base"}) {
		t.Fatalf("advertised %v, want [base]: TEMPLATE= must not reach the wire", got)
	}
	for _, id := range got {
		if id == "unbuilt" {
			t.Errorf("advertised %q, which is named in TEMPLATE= but has no rootfs on disk — "+
				"this claims the preferred (holds-it) scheduling tier for an image this node cannot boot", id)
		}
	}
}

func TestLocalTemplatesReportsNothingWhenNothingIsBuilt(t *testing.T) {
	t.Setenv("TEMPLATE", "base,code-interpreter")
	if got := LocalTemplates("firecracker", func() []string { return nil })(); len(got) != 0 {
		t.Errorf("a node with an empty image directory advertised %v, want nothing", got)
	}
}

func TestLocalTemplatesRescansPerBeat(t *testing.T) {
	ids := []string{"base"}
	report := LocalTemplates("firecracker", func() []string { return ids })
	if got := report(); !reflect.DeepEqual(got, []string{"base"}) {
		t.Fatalf("first beat = %v, want [base]", got)
	}
	ids = []string{"base", "code-interpreter"}
	if got := report(); !reflect.DeepEqual(got, []string{"base", "code-interpreter"}) {
		t.Errorf("second beat = %v, want the newly published image too — the scan was captured once", got)
	}
}

// A docker node pulls on demand and must advertise nil, which the scheduler reads as unrestricted.
func TestLocalTemplatesUnrestrictedForPullingRuntimes(t *testing.T) {
	for _, name := range []string{"docker", "process", ""} {
		if got := LocalTemplates(name, func() []string {
			t.Errorf("runtime %q scanned the Firecracker image directory", name)
			return []string{"base"}
		}); got != nil {
			t.Errorf("runtime %q returned a reporter; want nil (unrestricted)", name)
		}
	}
}

// Both node mains must wire the heartbeat through LocalTemplates; the two had
// drifted into identical copies of the same mistake before.
func TestBothNodeMainsAdvertiseTheSameWay(t *testing.T) {
	want := "nodeboot.LocalTemplates(runtime.Name(), templates.PublishedIDs)"
	assign := regexp.MustCompile(`(?m)^\s*bootable\s*:?=\s*(.+)$`)
	for _, main := range []string{"../../cmd/sandboxd/main.go", "../../cmd/sandboxd-fleet/main.go"} {
		src, err := os.ReadFile(main)
		if err != nil {
			t.Fatalf("read %s: %v", main, err)
		}
		if strings.Contains(string(src), "UnionStrings") {
			t.Errorf("%s unions the advertised template list with something again; "+
				"the advertised set is the disk scan alone", main)
		}
		m := assign.FindStringSubmatch(string(src))
		if m == nil {
			t.Fatalf("%s: no `bootable` assignment found — this guard is passing on an empty set; "+
				"the heartbeat wiring moved and the check must move with it", main)
		}
		if got := strings.TrimSpace(m[1]); got != want {
			t.Errorf("%s advertises via %q, want %q", main, got, want)
		}
	}
}

func TestParseLabels(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"", nil},
		{"a=1", map[string]string{"a": "1"}},
		{"a=1, b = 2 ,broken,=x", map[string]string{"a": "1", "b": "2"}},
		{",,", nil},
	}
	for _, tc := range cases {
		if got := parseLabels(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseLabels(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The scheduler reads null as "boots anything", the preferred tier, so an empty scan must send [].
func TestAnEmptyScanIsSentAsAnEmptyListNotNull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bootable func() []string
		want     string // heartbeat body value for "templates"
	}{
		{"does not enumerate", nil, `absent`},
		{"enumerates, nothing built", func() []string { return nil }, `[]`},
		{"enumerates, empty slice", func() []string { return []string{} }, `[]`},
		{"holds one", func() []string { return []string{"base"} }, `["base"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{}
			if ids, answering := advertisedTemplates(tc.bootable); answering {
				payload["templates"] = ids
			}
			got := "absent"
			if v, ok := payload["templates"]; ok {
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				got = string(b)
			}
			if got != tc.want {
				t.Errorf("heartbeat sent templates=%s, want %s — `null` and an absent field both "+
					"decode as \"boots anything\", which is the preferred scheduling tier", got, tc.want)
			}
		})
	}
}

func TestNodeClassComesFromTheAdvertisedLabels(t *testing.T) {
	t.Setenv("NODE_LABELS", "zone=eu, class=gpu-a100 ,tier=spot")
	if got := NodeClass(); got != "gpu-a100" {
		t.Errorf("NodeClass() = %q, want gpu-a100", got)
	}
	if got := parseLabels(os.Getenv("NODE_LABELS"))["class"]; got != NodeClass() {
		t.Errorf("NodeClass() disagrees with the advertised labels: %q vs %q", NodeClass(), got)
	}
}

func TestNodeClassIsEmptyWhenUnset(t *testing.T) {
	t.Setenv("NODE_LABELS", "")
	if got := NodeClass(); got != "" {
		t.Errorf("NodeClass() = %q, want empty", got)
	}
	t.Setenv("NODE_LABELS", "zone=eu")
	if got := NodeClass(); got != "" {
		t.Errorf("NodeClass() with no class label = %q, want empty", got)
	}
}

// The heartbeat's JSON keys are read by internal/orchestrator's heartbeatBody
// struct tags across the tier line, so a rename here is silent: every node
// reads as incapable and placement stops filtering. The orchestrator's
// TestTheHeartbeatKeysReachTheFleetAnswer pins the other half.
func TestTheHeartbeatSendsTheCapabilityKeysTheControlPlaneReads(t *testing.T) {
	src, err := os.ReadFile("heartbeat.go")
	if err != nil {
		t.Fatal(err)
	}
	for key, caps := range map[string]string{
		"tag_networks":   "caps.TagNetworks",
		"checkpoint":     "caps.Checkpoint",
		"named_snapshot": "caps.NamedSnapshot",
		"standby":        "caps.Standby",
	} {
		// Whitespace-tolerant: gofmt aligns map literals and would otherwise
		// break this guard on an unrelated edit.
		if !regexp.MustCompile(`"` + key + `":\s*` + regexp.QuoteMeta(caps)).Match(src) {
			t.Errorf(`the payload no longer sends "%s": %s — the control plane reads that exact `+
				`key, so the fleet would report this capability false on every node`, key, caps)
		}
	}
}
