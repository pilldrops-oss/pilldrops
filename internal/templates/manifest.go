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

// AgentMeta is the per-agent-CLI launch knowledge an agent template ships.
// Every command field is empty unless the flag was verified against the pinned
// version; callers must fall back rather than guess, since a wrong flag
// silently starts a new conversation or runs ungated.
type AgentMeta struct {
	// KeyEnv is empty for provider-agnostic agents; KeyNote then guides the user.
	KeyEnv   string `json:"key_env,omitempty"`
	KeyLabel string `json:"key_label,omitempty"`
	KeyNote  string `json:"key_note,omitempty"`
	// BaseImage is the image InstallCmd runs on; imageFor reads it too.
	BaseImage string `json:"base_image,omitempty"`
	// RunCmd runs a one-shot task; {task} is substituted.
	RunCmd string `json:"run_cmd,omitempty"`
	// ContinueCmd runs {task} as a follow-up turn in the most recent conversation.
	ContinueCmd     string `json:"continue_cmd,omitempty"`
	TaskPlaceholder string `json:"task_placeholder,omitempty"`
	CLIBin          string `json:"cli_bin,omitempty"`
	// InstallCmd is pinned so the capability claims in this struct stay falsifiable.
	InstallCmd string `json:"install_cmd,omitempty"`
	RunFlags   string `json:"run_flags,omitempty"`
	// RunFlagsEnv is "NAME=VALUE" environment RunFlags requires, applied only alongside it.
	RunFlagsEnv string `json:"run_flags_env,omitempty"`
	// StreamCmd runs {task} with structured event output on stdout.
	StreamCmd string `json:"stream_cmd,omitempty"`
	// StreamContinueCmd continues the latest conversation with structured
	// output. Two verified flags do not make a verified combination, so it is
	// set only where the pair was checked together.
	StreamContinueCmd string `json:"stream_continue_cmd,omitempty"`
	// StreamFormat names the event dialect; the dialects share no first frame,
	// and codex interleaves plain-text lines a parser must skip.
	StreamFormat string `json:"stream_format,omitempty"`
	// PermissionTool routes tool-approval decisions to a tool we serve; {tool} is the name.
	PermissionTool string `json:"permission_tool,omitempty"`
	// ResumeCmd resumes a specific conversation; {session} and {task} are substituted.
	ResumeCmd string `json:"resume_cmd,omitempty"`
	// ModelsCmd lists models one per line. Empty must render as "not supported", not an empty list.
	ModelsCmd string `json:"models_cmd,omitempty"`
	// CostSource is "stream" when events carry cost; empty must render as "not reported", not zero.
	CostSource string `json:"cost_source,omitempty"`

	// BaseURLEnv and ModelEnv are empty for agents that keep these in their config file.
	BaseURLEnv string `json:"base_url_env,omitempty"`
	ModelEnv   string `json:"model_env,omitempty"`
	// ConfigExample must be schema-correct; an example that does not load is worse than none.
	ConfigFile    string `json:"config_file,omitempty"`
	ConfigLabel   string `json:"config_label,omitempty"`
	ConfigFormat  string `json:"config_format,omitempty"`
	ConfigExample string `json:"config_example,omitempty"`
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

// agentMeta holds the per-agent-CLI launch knowledge. Empty: no catalog
// template is an agent CLI.
var agentMeta = map[string]AgentMeta{}

// AgentMetaByID returns a copy of the agent registry.
func AgentMetaByID() map[string]AgentMeta {
	out := make(map[string]AgentMeta, len(agentMeta))
	for id, m := range agentMeta {
		out[id] = m
	}
	return out
}

// AgentMetaFor looks up one agent without copying the whole registry.
func AgentMetaFor(id string) (AgentMeta, bool) {
	m, ok := agentMeta[id]
	return m, ok
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
		if a, ok := agentMeta[t.ID]; ok {
			am := a
			t.Agent = &am
		}
	}
}
