package templates

import "testing"

func TestPackageOfReadsEitherPhrasing(t *testing.T) {
	if got := InstallPackageOf("npm i -g @ampcode/cli"); got != "@ampcode/cli" {
		t.Fatalf(`InstallPackageOf("npm i -g @ampcode/cli") = %q`, got)
	}
	if got := InstallPackageOf("npm install -g opencode-ai"); got != "opencode-ai" {
		t.Fatalf(`InstallPackageOf("npm install -g opencode-ai") = %q`, got)
	}
}

func TestInstallPackageOfRecognizesProjectInstallForms(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"npm short flag", "npm i -g @ampcode/cli", "@ampcode/cli"},
		{"npm long flag", "npm install -g opencode-ai", "opencode-ai"},
		{"pip", "pip install aider-chat", "aider-chat"},
		{"pip3", "pip3 install open-interpreter", "open-interpreter"},
		{"pip with a leading flag", "pip install --no-cache-dir aider-chat", "aider-chat"},
		{"curl-piped binary installer names no package", "curl -fsSL https://get.goose.dev | sh", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InstallPackageOf(tc.cmd); got != tc.want {
				t.Errorf("InstallPackageOf(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

// A curl-piped binary install is tolerated rather than rejected for lacking a package.
func TestSyntheticPipAgentPassesInstallCommandCheck(t *testing.T) {
	synthetic := map[string]AgentMeta{
		"aider-synthetic": {InstallCmd: "pip install aider-chat"},
		"goose-synthetic": {InstallCmd: "curl -fsSL https://get.goose.dev | sh"},
	}
	for id, m := range synthetic {
		if m.InstallCmd == "" {
			t.Errorf("%s has no InstallCmd", id)
			continue
		}
		if IsPackageManagerInstall(m.InstallCmd) && InstallPackageOf(m.InstallCmd) == "" {
			t.Errorf("%s InstallCmd %q looks like a package-manager install but names no package — the false failure the fix removes", id, m.InstallCmd)
		}
	}
}

func TestEveryAgentHasAnInstallCommand(t *testing.T) {
	for id, m := range AgentMetaByID() {
		if m.InstallCmd == "" {
			t.Errorf("%s has no InstallCmd — nothing would install its CLI", id)
			continue
		}
		if IsPackageManagerInstall(m.InstallCmd) && InstallPackageOf(m.InstallCmd) == "" {
			t.Errorf("%s InstallCmd %q looks like a package-manager install but names no package", id, m.InstallCmd)
		}
	}
}

func TestEveryAgentDeclaresABaseImage(t *testing.T) {
	for id, m := range AgentMetaByID() {
		if m.BaseImage == "" {
			t.Errorf("%s declares no BaseImage", id)
		}
	}
}
