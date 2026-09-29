// Command agentd is the in-sandbox agent supervisor. It runs beside envd, owns
// the turn queue and event journal, and serves them to the node. Configuration
// comes from the template's launch manifest via -template; the per-field flags
// are overrides for testing and uncatalogued images.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd/parse"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func main() {
	var (
		template = flag.String("template", "",
			"template id; its launch manifest supplies the agent's commands")
		checkOnly = flag.Bool("check", false,
			"exit 0 if -template declares an agent, non-zero otherwise; for the build script")
		stateDir = flag.String("state-dir", "/var/lib/agentd",
			"where the journal and queue live; must be on the writable overlay")
		workDir = flag.String("work-dir", "/root", "working directory for agent turns")
		shell   = flag.String("shell", "/bin/sh", "shell used to run turns")
		// Applied only when positive so a restart never clears a cap set through the API.
		spendCap = flag.Float64("spend-cap-usd", 0,
			"cap on reported model spend for this workstation; 0 leaves it unset")
		listen = flag.String("listen", "",
			"listen address; empty picks 0.0.0.0 when a token hash is present, loopback otherwise")
		// Overrides. Empty means "take it from the template manifest".
		runCmd       = flag.String("run-cmd", "", "override AgentMeta.RunCmd")
		continueCmd  = flag.String("continue-cmd", "", "override AgentMeta.ContinueCmd")
		streamCmd    = flag.String("stream-cmd", "", "override AgentMeta.StreamCmd")
		agentVersion = flag.String("agent-version", "", "agent CLI version, recorded per turn")
		// Never set by the build script; it must be typed so it shows in the process list.
		allowExposed = flag.Bool("allow-unauthenticated-exposure", false,
			"DANGEROUS: serve on a routable address with no token (local testing only)")
	)
	gateMode := flag.Bool("gate", false,
		"run as the approval-gate shim: relay MCP stdio to the supervisor's gate socket")
	flag.Parse()

	// -gate is the stdio relay claude-code spawns; it must not open the journal,
	// queue or listener, so handle it before anything is initialised.
	if *gateMode {
		if err := agentd.RunGateShim("", os.Stdin, os.Stdout); err != nil {
			// stdout is the MCP channel; a log line there is a malformed frame.
			fmt.Fprintf(os.Stderr, "agentd -gate: %v\n", err)
			os.Exit(1)
		}
		return
	}

	meta, tmplID, err := resolveAgent(*template, *runCmd, *continueCmd, *streamCmd)
	if *checkOnly {
		if err != nil {
			os.Exit(1)
		}
		fmt.Println(tmplID)
		return
	}
	if err != nil {
		log.Fatalf("agentd: %v", err)
	}

	red := agentd.NewRedactor()
	// Register secrets before anything is journaled; the journal is mirrored off the box.
	for _, name := range secretNames(meta) {
		if v := os.Getenv(name); v != "" {
			red.AddSecret(name, v)
		}
	}

	journal, err := agentd.OpenJournal(*stateDir+"/journal", red)
	if err != nil {
		log.Fatalf("agentd: journal: %v", err)
	}
	// Closed at the end of main; the Fatalf paths below would skip a defer anyway.

	queue, err := agentd.OpenQueue(*stateDir + "/queue")
	if err != nil {
		log.Fatalf("agentd: queue: %v", err)
	}

	budget, err := agentd.OpenBudget(*stateDir + "/budget.json")
	if err != nil {
		log.Fatalf("agentd: budget: %v", err)
	}
	if *spendCap > 0 {
		budget.SetCap(*spendCap)
	}

	mode := agentd.ModeBasic
	if meta.StreamCmd != "" && meta.StreamFormat != "" {
		mode = agentd.ModeRich
	}

	builder := &agentd.Templates{
		RunCmd:            meta.RunCmd,
		ContinueCmd:       meta.ContinueCmd,
		StreamCmd:         meta.StreamCmd,
		StreamContinueCmd: meta.StreamContinueCmd,
		RunFlags:          meta.RunFlags,
		RunFlagsEnv:       meta.RunFlagsEnv,
		CLIBin:            meta.CLIBin,
		ModelsCmd:         meta.ModelsCmd,
		Env:               os.Environ(),
		Shell:             *shell,
		Dir:               *workDir,
	}

	// No parser means plain capture; downgrade mode so status never claims a
	// timeline the sandbox cannot deliver.
	parser := parse.For(meta.StreamFormat)
	if parser == nil && mode == agentd.ModeRich {
		log.Printf("agentd: no parser for stream format %q — running in basic mode", meta.StreamFormat)
		mode = agentd.ModeBasic
	}

	sup := &agentd.Supervisor{
		Queue:   queue,
		Journal: journal,
		Runner: &agentd.Runner{
			Journal: journal, Build: builder.Build, Shell: *shell, Dir: *workDir,
			Parser: parser, Structured: builder.StructuredFor,
		},
		Mode: mode,
		// Validated by the supervisor against what this agent can do, not trusted here.
		DefaultAutonomy: strings.TrimSpace(os.Getenv("PILLDROPS_AGENT_MODE")),
		Budget:          budget,
		AgentVersion:    agentVersionOf(*agentVersion, meta.CLIBin),
	}

	// Gate only when the manifest carries a verified permission-tool flag. A gate
	// setup failure must never fall back to autonomous; Guided is left unavailable.
	var gateForAPI *agentd.Gate
	// Declared outside the block so the srv.Listen failure path can close it.
	var gateListener *agentd.GateListener
	if meta.PermissionTool != "" && gateWorthEstablishing(*shell, meta) {
		gate := agentd.NewGate(journal, agentd.DefaultPolicy)
		gate.Redactor = red
		// Park the turn in awaiting_approval so clients can tell waiting from thinking.
		gate.OnPark = func(turnID string, waiting bool) {
			state := agentd.TurnRunning
			if waiting {
				state = agentd.TurnAwaiting
			}
			// The turn may have been removed while the gate held it; the decision must still complete.
			_ = queue.SetState(turnID, state) //nolint:errcheck // deliberate, see comment above
		}
		// Persisted beside the journal so "remember: sandbox" survives a restart.
		gate.Remembered = agentd.OpenRememberStore(*stateDir + "/approvals.json")

		self, err := os.Executable()
		if err != nil {
			log.Printf("agentd: cannot locate own binary, so the approval gate is "+
				"unavailable and Guided mode is not offered: %v", err)
		} else if l, err := agentd.ListenGate(gate, *stateDir+"/gate.sock"); err != nil {
			log.Printf("agentd: approval gate socket unavailable, so Guided mode is "+
				"not offered: %v", err)
		} else if cfg, err := agentd.WriteGateConfig(*stateDir, self, l.Path()); err != nil {
			log.Printf("agentd: cannot write the gate's MCP config, so Guided mode is "+
				"not offered: %v", err)
			closeGateListener(l)
		} else {
			builder.PermissionTool = meta.PermissionTool
			builder.GateConfig = cfg
			gateForAPI = gate
			// Read through the builder so status and the built command agree on Guided.
			sup.GuidedAvailable = builder.GuidedAvailable
			// The supervisor compares this across a Guided turn to catch a CLI that ignores the flag.
			sup.GateConsulted = gate.Consulted
			// One MCP connection spans many turns, and turns run one at a time, so a
			// single current-turn value is correct.
			sup.Runner.OnTurnStart = l.SetTurn
			gateListener = l
			log.Printf("agentd: approval gate ready — Guided mode available (policy: %d rules)",
				len(agentd.DefaultPolicy.Rules))
		}
	}

	// Surface a missing agent binary once in status instead of as every task failing.
	if meta.CLIBin != "" {
		if _, err := exec.LookPath(meta.CLIBin); err != nil {
			sup.NoteAgentRestart(fmt.Sprintf("the agent binary %q is not on PATH", meta.CLIBin))
		}
	}

	tokenHash := readTokenHash()
	addr := chooseAddr(*listen, tokenHash, *allowExposed)

	srv := &agentd.Server{
		Sup: sup, Journal: journal, Queue: queue,
		Addr: addr, TokenHash: tokenHash, Gate: gateForAPI,
		CurrentModel: func() string { return currentModel(meta) },
	}
	// Nil when there is no verified listing command; the handler reports unsupported.
	if meta.ModelsCmd != "" {
		srv.Models = builder.Models
	}
	ln, httpSrv, err := srv.Listen()
	if err != nil {
		// Remove the gate socket file so the next restart can bind the same path.
		closeGateListener(gateListener)
		log.Fatalf("agentd: %v", err)
	}
	defer closeGateListener(gateListener)

	// Drain on SIGTERM so no turn is left marked running.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("agentd: http: %v", err)
		}
	}()
	log.Printf("agentd: template=%s agent=%s mode=%s listening=%s auth=%s",
		tmplID, meta.CLIBin, mode, addr, authState(tokenHash))

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		if err := sup.Run(ctx); err != nil {
			log.Printf("agentd: supervisor: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("agentd: stopping")

	// The journal fsyncs at turn boundaries, so cutting this short loses a turn's tail.
	select {
	case <-loopDone:
	case <-time.After(30 * time.Second):
		log.Print("agentd: supervisor did not stop in time; shutting down anyway")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("agentd: http shutdown: %v", err)
	}

	// Best-effort: turn-boundary syncs already cover durability.
	if err := journal.Close(); err != nil {
		log.Printf("agentd: journal close: %v", err)
	}
}

// resolveAgent loads the template's agent metadata and applies overrides.
// A non-agent template is an error, since agentd is only baked into agent templates.
func resolveAgent(id, runCmd, continueCmd, streamCmd string) (templates.AgentMeta, string, error) {
	var meta templates.AgentMeta
	if id != "" {
		tmpl, ok := templates.Resolve(id)
		if !ok {
			return meta, id, fmt.Errorf("unknown template %q", id)
		}
		if tmpl.Kind != templates.KindAgent || tmpl.Agent == nil {
			return meta, id, fmt.Errorf("template %q is kind %q, not an agent", id, tmpl.Kind)
		}
		meta = *tmpl.Agent
	}
	if runCmd != "" {
		meta.RunCmd = runCmd
	}
	if continueCmd != "" {
		meta.ContinueCmd = continueCmd
	}
	if streamCmd != "" {
		meta.StreamCmd = streamCmd
	}
	if meta.RunCmd == "" {
		return meta, id, fmt.Errorf("no run command: pass -template or -run-cmd")
	}
	return meta, id, nil
}

// secretNames lists the env vars whose values must never reach the journal.
func secretNames(meta templates.AgentMeta) []string {
	names := []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GROK_API_KEY", "XAI_API_KEY",
		"AMP_API_KEY", "OPENROUTER_API_KEY", "GOOGLE_API_KEY", "GEMINI_API_KEY",
		"MISTRAL_API_KEY", "GITHUB_TOKEN", "GH_TOKEN",
		// Tool providers named by Hermes' configuration panel.
		"EXA_API_KEY", "TAVILY_API_KEY", "FIRECRAWL_API_KEY", "BROWSERBASE_API_KEY",
		"BROWSER_USE_API_KEY", "FAL_KEY", "PARALLEL_API_KEY",
		"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	}
	if meta.KeyEnv != "" {
		names = append(names, meta.KeyEnv)
	}
	return names
}

// agentVersionOf returns the agent CLI version, asking the binary when not supplied.
func agentVersionOf(given, bin string) string {
	if given != "" {
		return given
	}
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// readTokenHash returns the SHA-256 of this sandbox's access token. It reads the
// same sources as cmd/envd/authtoken.go: one token covers both services.
func readTokenHash() string {
	if h := strings.TrimSpace(os.Getenv("ENVD_TOKEN_HASH")); h != "" {
		return h
	}
	if b, err := os.ReadFile("/proc/cmdline"); err == nil {
		for _, tok := range strings.Fields(string(b)) {
			if v, ok := strings.CutPrefix(tok, "envd.token_hash="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// chooseAddr picks the listen address. Without a token hash it binds loopback:
// this surface can run processes, so it must not sit unauthenticated on the
// shared bridge.
func chooseAddr(explicit, tokenHash string, allowExposed bool) string {
	if explicit != "" {
		return explicit
	}
	if tokenHash != "" {
		return fmt.Sprintf("0.0.0.0:%d", agentd.DefaultPort)
	}
	if allowExposed {
		log.Printf("agentd: WARNING serving on every interface with NO token — "+
			"any sandbox on this network can drive this agent (port %d)", agentd.DefaultPort)
		return fmt.Sprintf("0.0.0.0:%d", agentd.DefaultPort)
	}
	log.Print("agentd: no access-token hash provisioned; binding loopback only. " +
		"The node cannot reach the agent API — enable per-sandbox tokens (EnvdAuth), " +
		"or pass -allow-unauthenticated-exposure for local testing.")
	return agentd.LoopbackAddr
}

// currentModel returns the configured model from its env var, or "" when the
// agent keeps it in a config file that is not parsed.
func currentModel(meta templates.AgentMeta) string {
	if meta.ModelEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(meta.ModelEnv))
}

func authState(hash string) string {
	if hash == "" {
		return "OFF"
	}
	return "token"
}

// closeGateListener closes the approval-gate socket; nil is a no-op.
func closeGateListener(l *agentd.GateListener) {
	if l == nil {
		return
	}
	if err := l.Close(); err != nil {
		log.Printf("agentd: gate listener close: %v", err)
	}
}

// gateWorthEstablishing probes whether the CLI accepts the permission flag.
// Inconclusive returns true: the probe can fail for unrelated reasons, and the
// per-turn audit catches an ignored flag.
func gateWorthEstablishing(shell string, meta templates.AgentMeta) bool {
	probe := agentd.ProbePermissionTool(context.Background(), shell, meta.CLIBin, meta.PermissionTool)
	switch {
	case probe.Accepted:
		return true
	case probe.Inconclusive:
		log.Printf("agentd: permission-flag probe inconclusive (%s) — Guided stays available "+
			"and the per-turn gate audit will catch an ignored flag", probe.Reason)
		return true
	default:
		log.Printf("agentd: %s — the approval gate will not be established and Guided "+
			"mode will not be offered", probe.Reason)
		return false
	}
}
