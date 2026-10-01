package sandbox

import (
	"errors"
	"testing"
	"time"
)

var freePlan = Limits{
	ID: "free", MaxVCPU: 2, MaxMemMB: 2048, MaxStorageMB: 5120,
	MaxConcurrent: 2, SessionTTLHrs: 4,
}

var proPlan = Limits{
	ID: "pro", MaxVCPU: 8, MaxMemMB: 16384, MaxStorageMB: 51200,
	MaxConcurrent: 20, MaxWorkstations: 1, SessionTTLHrs: 24, Persistent: true,
}

func TestResourcesClampToThePlanCeiling(t *testing.T) {
	got, err := Resolve(
		Spec{Template: "python-3.11", VCPU: 64, MemoryMB: 999999, StorageMB: 999999},
		TemplateDefaults{ID: "code-interpreter"}, freePlan, Fleet{}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.VCPU != freePlan.MaxVCPU || got.MemMiB != freePlan.MaxMemMB || got.StorageMB != freePlan.MaxStorageMB {
		t.Errorf("got %d vcpu / %d MB / %d MB, want the free plan's ceiling %d/%d/%d",
			got.VCPU, got.MemMiB, got.StorageMB,
			freePlan.MaxVCPU, freePlan.MaxMemMB, freePlan.MaxStorageMB)
	}
}

func TestResourcesFallBackToTheTemplateThenTheFloor(t *testing.T) {
	got, _ := Resolve(Spec{Template: "t"},
		TemplateDefaults{ID: "t", CPU: 2, MemoryMB: 1024, StorageMB: 2048}, proPlan, Fleet{}, nil)
	if got.VCPU != 2 || got.MemMiB != 1024 || got.StorageMB != 2048 {
		t.Errorf("got %d/%d/%d, want the template's 2/1024/2048", got.VCPU, got.MemMiB, got.StorageMB)
	}
	got, _ = Resolve(Spec{Template: "t"}, TemplateDefaults{ID: "t"}, proPlan, Fleet{}, nil)
	if got.VCPU != minVCPU || got.MemMiB != minMemMB || got.StorageMB != minStorageMB {
		t.Errorf("got %d/%d/%d, want the floor %d/%d/%d",
			got.VCPU, got.MemMiB, got.StorageMB, minVCPU, minMemMB, minStorageMB)
	}
}

// An uncatalogued image-backed service must not land on the 128 MiB floor, which OOMs it during init.
func TestImageBackedServiceGetsAServiceSizedDefault(t *testing.T) {
	got, err := Resolve(Spec{Template: "mysql-8", Image: "mysql:8"},
		TemplateDefaults{}, proPlan, Fleet{}, nil) // no catalogue entry
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.MemMiB != serviceDefaultMemMB {
		t.Errorf("memory = %d MB, want %d — the floor OOM-kills a real service at init",
			got.MemMiB, serviceDefaultMemMB)
	}
	if got.VCPU != serviceDefaultVCPU {
		t.Errorf("vcpu = %d, want %d", got.VCPU, serviceDefaultVCPU)
	}
	got, _ = Resolve(Spec{Template: "mysql-8", Image: "mysql:8"}, TemplateDefaults{},
		Limits{ID: "tiny", MaxVCPU: 1, MaxMemMB: 512, MaxStorageMB: 1024}, Fleet{}, nil)
	if got.MemMiB != 512 || got.VCPU != 1 {
		t.Errorf("got %d vcpu / %d MB, want the tiny plan's 1/512", got.VCPU, got.MemMiB)
	}
}

func TestLifetimeRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    Spec
		plan    Limits
		fleet   Fleet
		want    time.Duration
		wantErr bool
	}{
		{
			name: "unset timeout gets the short renewable lease",
			spec: Spec{}, plan: proPlan,
			want: defaultLeaseSeconds * time.Second,
		},
		{
			name: "explicit timeout is honoured under the plan cap",
			spec: Spec{Timeout: 3600}, plan: proPlan,
			want: time.Hour,
		},
		{
			name: "a request past the plan's session TTL is capped, not refused",
			spec: Spec{Timeout: 999999999}, plan: freePlan,
			want: 4 * time.Hour,
		},
		{
			name: "negative timeout on a persistent plan means no expiry",
			spec: Spec{Timeout: -1}, plan: proPlan,
			want: -1,
		},
		{
			name: "negative timeout on a NON-persistent plan degrades to the lease",
			spec: Spec{Timeout: -1}, plan: freePlan,
			want: defaultLeaseSeconds * time.Second,
		},
		{
			name: "the workstation cap is enforced separately from concurrency",
			spec: Spec{Timeout: -1}, plan: proPlan,
			fleet:   Fleet{PersistentCount: 1}, // pro sells exactly 1
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.spec, TemplateDefaults{ID: "t"}, tc.plan, tc.fleet, nil)
			if tc.wantErr {
				var le *LimitError
				if !errors.As(err, &le) {
					t.Fatalf("err = %v, want a *LimitError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.TTL != tc.want {
				t.Errorf("TTL = %v, want %v", got.TTL, tc.want)
			}
		})
	}
}

func TestIdleTimeoutRules(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want time.Duration
	}{
		{0, 0},                         // platform default
		{-1, -1},                       // never pause
		{-9999, -1},                    // any negative means never
		{5, idleAutoPauseFloor},        // floored so it cannot thrash
		{300, 300 * time.Second},       // honoured in range
		{999999, idleAutoPauseCeiling}, // capped
	} {
		got, err := Resolve(Spec{Template: "t", IdleTimeout: tc.in},
			TemplateDefaults{ID: "t"}, proPlan, Fleet{}, nil)
		if err != nil {
			t.Fatalf("resolve(%d): %v", tc.in, err)
		}
		if got.IdleTimeout != tc.want {
			t.Errorf("idle_timeout %d resolved to %v, want %v", tc.in, got.IdleTimeout, tc.want)
		}
	}
}

// The VM layer looks up a rootfs by canonical id, so an unresolved alias fails the boot.
func TestTemplateAliasResolvesToTheCanonicalID(t *testing.T) {
	got, _ := Resolve(Spec{Template: "python-3.11"},
		TemplateDefaults{ID: "code-interpreter"}, proPlan, Fleet{}, nil)
	if got.TemplateID != "code-interpreter" {
		t.Errorf("TemplateID = %q, want the canonical %q", got.TemplateID, "code-interpreter")
	}
}

func TestResolveDoesNotMutateTheCallerEnv(t *testing.T) {
	env := []string{"A=1"}
	_, _ = Resolve(Spec{Template: "t", Env: env},
		TemplateDefaults{ID: "t"}, proPlan, Fleet{}, nil)
	if len(env) != 1 || env[0] != "A=1" {
		t.Errorf("caller's env was mutated: %v", env)
	}
}

func TestAppPortResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		tmpl TemplateDefaults
		want int
	}{
		{"explicit port wins", Spec{Port: 3000, Start: "x"}, TemplateDefaults{Port: 8080}, 3000},
		{"start command with no port gets the default", Spec{Start: "python app.py"}, TemplateDefaults{}, defaultStartPort},
		{"no start command publishes the template's port", Spec{}, TemplateDefaults{Port: 8080}, 8080},
		{"neither publishes nothing", Spec{}, TemplateDefaults{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Resolve(tc.spec, tc.tmpl, proPlan, Fleet{}, nil)
			if got.Port != tc.want {
				t.Errorf("port = %d, want %d", got.Port, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	known := func(s string) bool { return s == "python-3.11" }
	nameOK := func(s string) (string, error) { return s, nil }
	repoOK := func(url, ref string) error { return nil }

	if _, err := (Spec{Template: "python-3.11"}).Validate(known, nameOK, repoOK); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
	_, err := (Spec{Template: "nope"}).Validate(known, nameOK, repoOK)
	var ie *InvalidError
	if !errors.As(err, &ie) {
		t.Errorf("unknown template: err = %v, want *InvalidError", err)
	}
	// An image-backed create skips the catalogue check.
	if _, err := (Spec{Template: "nope", Image: "postgres:16"}).Validate(known, nameOK, repoOK); err != nil {
		t.Errorf("image-backed create rejected: %v", err)
	}
}
