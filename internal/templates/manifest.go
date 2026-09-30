package templates

// Template kinds drive the launch form, the CTA and the post-launch workspace.
const (
	KindShell   = "shell"
	KindCode    = "code"
	KindApp     = "app"
	KindAgent   = "agent"
	KindScreen  = "screen"
	KindTest    = "test"
	KindVerdict = "verdict"
	KindService = "service"
)

// Kinds lists every valid kind.
var Kinds = []string{
	KindShell, KindCode, KindApp, KindAgent, KindScreen, KindTest, KindVerdict, KindService,
}

// PrimaryInput is the one thing a template's quick-launch asks for; nil means
// a zero-field launch. Type is one of code, url, target, apk, script, tests, task.
type PrimaryInput struct {
	Type     string `json:"type"`
	Label    string `json:"label"`
	Required bool   `json:"required,omitempty"`
}

// launchManifests assigns kind, outcome and primary input per catalog id.
// Outcome is one of terminal, preview, screen, report, verdict, endpoint, connection.
var launchManifests = map[string]struct {
	Kind    string
	Outcome string
	Primary *PrimaryInput
}{
	"base": {Kind: KindShell, Outcome: "terminal"},

	// A shell whose point is the container daemon already running in it; the
	// preview shows whatever the user publishes on 8000.
	"docker": {Kind: KindShell, Outcome: "terminal"},

	"code-interpreter": {Kind: KindCode, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "code", Label: "Code or data — optional, opens a REPL without it"}},
	"node": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project — optional, a working starter is included"}},
	"go": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project — optional, a working starter is included"}},
	"rust": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project (cargo)"}},
	"browser": {Kind: KindScreen, Outcome: "screen",
		Primary: &PrimaryInput{Type: "url", Label: "Start URL — optional"}},
	"desktop": {Kind: KindScreen, Outcome: "screen",
		Primary: &PrimaryInput{Type: "url", Label: "Start URL — optional"}},
	"playwright": {Kind: KindTest, Outcome: "report",
		Primary: &PrimaryInput{Type: "tests", Label: "Tests — repo, zip or a spec file (demo suite included)"}},
}

// init decorates the compiled-in catalog with its launch manifests.
func init() {
	for i := range catalog {
		t := &catalog[i]
		if m, ok := launchManifests[t.ID]; ok {
			t.Kind = m.Kind
			t.Outcome = m.Outcome
			t.PrimaryInput = m.Primary
		}
	}
}
