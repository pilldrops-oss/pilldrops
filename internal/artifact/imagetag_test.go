package artifact

import "testing"

// Both tiers resolve a published custom image through this rule.
func TestTemplateIDForImageTag(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want string
		ok   bool
	}{
		// What customImageTag produces.
		{"sf-acme-mytool:latest", "sf-acme-mytool", true},
		{"sf-default-my-tool.v2:latest", "sf-default-my-tool.v2", true},
		{"python:3.11", "python", true},
		{"alpine", "alpine", true},
		{"ghcr.io/acme/tool:v1", "ghcr.io_acme_tool", true},
		// A registry port is not a version.
		{"registry.local:5000/acme/tool:v1", "registry.local:5000_acme_tool", false},
		{"acme/tool@sha256:" + "ab12" + "34cd", "", false},
		{"", "", false},
		{"../escape:latest", "", false},
	} {
		got, ok := TemplateIDForImageTag(tc.tag)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("TemplateIDForImageTag(%q) = (%q, %v), want (%q, %v)",
				tc.tag, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAPublishedImageIsNotMistakenForARootfs(t *testing.T) {
	if NameImage == NameRootfs {
		t.Fatal("the Docker and Firecracker payloads must not share an artifact name")
	}
	img, err := Key("bld-1", NameImage)
	if err != nil {
		t.Fatal(err)
	}
	root, err := Key("bld-1", NameRootfs)
	if err != nil {
		t.Fatal(err)
	}
	if img == root {
		t.Fatalf("both payloads key to %q", img)
	}

	// A missing payload field defaults to rootfs for pre-custom-image builds.
	var old Metadata
	if old.PayloadName() != NameRootfs {
		t.Errorf("a metadata.json with no payload field reads as %q, want %q "+
			"— every build published before §6 has no such field",
			old.PayloadName(), NameRootfs)
	}
	if (Metadata{Payload: NameImage}).PayloadName() != NameImage {
		t.Error("an explicit payload must be reported as itself")
	}
}
