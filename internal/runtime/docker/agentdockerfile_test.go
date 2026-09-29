package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// TestOnDiskAgentDockerfilesMatchTheManifest checks the checked-in
// templates/<agent>/Dockerfile, which nothing derives from the manifest, so it
// can genuinely drift. Packages are compared via InstallPackageOf because the
// two sides phrase the install command differently ("npm i" vs "npm install").
func TestOnDiskAgentDockerfilesMatchTheManifest(t *testing.T) {
	root := filepath.Join("..", "..", "..", "templates")
	for id, m := range templates.AgentMetaByID() {
		path := filepath.Join(root, id, "Dockerfile")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			// scripts/build-template.sh hard-fails without one.
			t.Errorf("%s: no checked-in Dockerfile — scripts/build-template.sh cannot build a Firecracker rootfs for %q without one", path, id)
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		df := string(raw)

		if !strings.Contains(df, "FROM "+m.BaseImage) {
			t.Errorf("%s is not FROM the manifest's base %q", path, m.BaseImage)
		}

		wantPkg := templates.InstallPackageOf(m.InstallCmd)
		if wantPkg == "" {
			// A curl|sh install names no package; only fail if the parser should have found one.
			if templates.IsPackageManagerInstall(m.InstallCmd) {
				t.Errorf("%s: manifest InstallCmd %q looks like a package-manager install but names no package to check %s against", id, m.InstallCmd, path)
			}
			continue
		}
		if gotPkg := templates.InstallPackageOf(df); gotPkg != wantPkg {
			t.Errorf("%s installs %q, but the manifest's InstallCmd for %s names %q", path, gotPkg, id, wantPkg)
		}
	}
}

// TestAgentdRunsInTheProductWorkdir checks agentd is launched with -work-dir
// containerWorkdir; its own default is /root, where the user's files are not.
func TestAgentdRunsInTheProductWorkdir(t *testing.T) {
	body := sourceContaining(t, `/usr/local/bin/agentd -template`)
	i := strings.Index(body, "/usr/local/bin/agentd -template")
	if i < 0 {
		t.Fatal("the agentd launch line has moved; this test no longer checks anything")
	}
	launch := body[i:min(i+400, len(body))]
	if !strings.Contains(launch, "-work-dir") {
		t.Error("agentd is launched with no -work-dir, so it falls back to /root and the agent " +
			"works somewhere the rest of the product does not use")
	}
	if !strings.Contains(launch, "containerWorkdir") {
		t.Error("the work dir is not containerWorkdir — a second literal here would drift from " +
			"the directory every other part of this runtime uses")
	}
}

// TestAgentDoesNotRunAsRoot checks the agentd launch drops to agentRunUser. The
// agent runs model-written commands; as root it could overwrite its own supervisor.
func TestAgentDoesNotRunAsRoot(t *testing.T) {
	body := sourceContaining(t, `/usr/local/bin/agentd -template`)
	i := strings.Index(body, "/usr/local/bin/agentd -template")
	if i < 0 {
		t.Fatal("the agentd launch has moved; this test no longer checks anything")
	}
	// setpriv must precede the binary in the command, or it is not wrapping it.
	launch := body[max(0, i-700):i]
	if !strings.Contains(launch, "setpriv") {
		t.Error("agentd is launched with no privilege drop, so the agent runs as root")
	}
	// Check the constant's name, not its value, so a spliced-in literal cannot drift from the chown.
	if !strings.Contains(launch, "agentRunUser") {
		t.Error("the launch does not use the agentRunUser constant")
	}
	if agentRunUser == "root" || agentRunUser == "" {
		t.Errorf("agentRunUser is %q", agentRunUser)
	}
	if !strings.Contains(launch, "agentRunHome") {
		t.Error("HOME is not set from agentRunHome, so the agent CLI writes its config into a " +
			"directory the unprivileged user cannot read — which fails in unrelated-looking ways")
	}
	if agentRunHome == "/root" || agentRunHome == "" {
		t.Errorf("agentRunHome is %q", agentRunHome)
	}
	if !strings.Contains(body, "-state-dir") {
		t.Error("no -state-dir passed, so agentd falls back to /var/lib/agentd, which an " +
			"unprivileged process cannot write to")
	}
}
