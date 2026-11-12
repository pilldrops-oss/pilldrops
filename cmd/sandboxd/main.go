// Command sandboxd is the single self-hosted sandbox server: daemon, store,
// preview proxy and dashboard behind one HTTP API. Boot helpers shared with
// cmd/sandboxd-fleet live in internal/nodeboot.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/dashboard"
	"github.com/pilldrops-oss/pilldrops/internal/nodeboot"
	"github.com/pilldrops-oss/pilldrops/internal/observability"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/retention"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
	"github.com/pilldrops-oss/pilldrops/internal/webui"
	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		slog.Error("refusing to start", "component", "sandboxd", "err", err)
		os.Exit(1)
	}
}

// run is the whole process. Boot guards return errors rather than calling
// os.Exit so tests can assert which misconfiguration is refused and defers run.
func run() error {
	slog.SetDefault(observability.NewLogger(os.Stderr, nodeboot.Env("LOG_FORMAT", "json"), nodeboot.Env("LOG_LEVEL", "info")))

	// The node API can exec into every sandbox, so a publicly routable node must
	// set BIND_ADDR or be firewalled to the control-plane path.
	addr := nodeboot.Env("BIND_ADDR", "") + ":" + nodeboot.Env("PORT", "8080")
	dataDir := nodeboot.Env("SANDBOXD_DATA", filepath.Join(os.TempDir(), "sandboxd"))
	if err := nodeboot.DataDir(dataDir); err != nil {
		return err
	}

	// Bounds boot-time store work so an unreachable database cannot hang boot.
	bootCtx, stopBoot := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopBoot()

	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	dsn := nodeboot.Env("SANDBOXD_DATABASE_URL", filepath.Join(dataDir, "state.db"))
	// One box answering its own API may keep SQLite; a node other nodes share a database with may not.
	clustered := nodeboot.Env("ORCHESTRATOR_URL", "") != ""
	if err := nodeboot.ProductionDBError(nodeboot.EnvBool("SANDBOXD_PRODUCTION", false) && clustered, dsn); err != nil {
		return err
	}
	st, err := store.Open(bootCtx, dsn)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer func() { _ = st.Close() }() // best-effort; process is exiting either way
	if err := st.SetSecretKey(nodeboot.Env("SANDBOXD_SECRET_KEY", "")); err != nil {
		return fmt.Errorf("secret key: %w", err)
	}
	// Register NODE_API_KEY as an operator key so the orchestrator's calls authenticate.
	if nk := nodeboot.Env("NODE_API_KEY", ""); nk != "" {
		if err := st.EnsureKeyWithValue(bootCtx, nk, "default", "operator"); err != nil {
			slog.Warn("could not register NODE_API_KEY", "component", "node", "err", err)
		}
	}

	if strings.HasPrefix(dsn, "postgres") {
		slog.Warn("using Postgres via SANDBOXD_DATABASE_URL; existing SQLite data is NOT migrated",
			"component", "store", "sqlite_dir", dataDir)
	}

	retention.Start(bootCtx, st)

	runtime, isolation := nodeboot.SelectRuntime(bootCtx, dataDir, logger)

	// SelectRuntime returns nil in a release build with no isolation, so every
	// use of runtime below depends on this guard returning first.
	devInsecure := nodeboot.EnvBool("SANDBOXD_DEV", false) || nodeboot.EnvBool("DEV_INSECURE", false)
	if err := nodeboot.IsolationGuardError(nodeboot.DevBuild, devInsecure, isolation); err != nil {
		return err
	}

	// A storage driver can accept --storage-opt size and ignore it, so the cap
	// is verified rather than assumed. Firecracker sizes its own overlay.
	if enforced, known, detail := nodeboot.CheckStorageCap(context.Background(), runtime); known || runtime.Name() == "docker" {
		if err := nodeboot.StorageCapGuardError(nodeboot.EnvBool("SANDBOXD_PRODUCTION", false), enforced, known, detail); err != nil {
			return err
		}
		if known && !enforced {
			slog.Warn("this host does NOT enforce per-sandbox disk caps; one sandbox can fill the node's "+
				"disk and storage is metered at a size nothing holds it to",
				"component", "runtime", "detail", detail)
		}
	}

	// A preflight failure is not fatal: the node stays up and diagnosable but
	// refuses creates and suppresses its heartbeat.
	pf := nodeboot.RunPreflight(context.Background(), runtime, isolation, nodeboot.VerifyEgress)
	preflightFailed := nodeboot.ReportPreflight(pf, runtime.Name())

	orchURL := nodeboot.Env("ORCHESTRATOR_URL", "")

	// Installed before the daemon, which needs the pin hook so a live VM's
	// rootfs is never evicted under disk pressure.
	tiers, err := nodeboot.InstallTemplateSource(bootCtx, dataDir, orchURL,
		nodeboot.Env("NODE_API_KEY", ""), st, nil)
	if err != nil {
		return fmt.Errorf("artifact store: %w", err)
	}
	pinTemplate, artifactCanFetch := tiers.PinTemplate, tiers.CanFetch
	// nil unless the store can hold a disk image; the daemon reads that as
	// "this node cannot take backups" and says so with a 501.
	var backupBlobs daemon.BlobStore
	if tiers.Backups != nil {
		backupBlobs = tiers.Backups
	}
	// Only the Docker runtime fetches published custom-image tarballs; the
	// others have no such concept, hence the type assertion.
	if src, ok := runtime.(interface {
		SetImageSource(func(context.Context, string) error)
	}); ok && tiers.FetchImage != nil {
		src.SetImageSource(tiers.FetchImage)
	}

	dashLogs := dashboard.NewLogBroadcaster()

	nodeID := nodeboot.NodeID()
	// A paused sandbox's snapshot dies with the node unless archived off it.
	snapArchive, err := nodeboot.SnapshotArchive(backupBlobs, dataDir)
	if err != nil {
		return err
	}

	d := daemon.New(daemon.Config{
		Runtime: runtime,
		Store:   daemon.StoreAdapter{S: st, NodeID: nodeID, NodeClass: nodeboot.NodeClass()},
		// Backups need both: the rows to index them and a store big enough to
		// hold a disk. Blobs is nil on a controlplane-backed node, and the
		// daemon then answers 501 naming the stores that would work.
		Backups:         st,
		Blobs:           backupBlobs,
		NodeID:          nodeID,
		SnapshotDir:     filepath.Join(dataDir, "snapshots"),
		SnapshotArchive: snapArchive,
		// idle_timeout freezes a sandbox in RAM (standby) and is off by default:
		// a node opts into holding idle RAM. hibernate_timeout is the disk tier,
		// and is where the old 5-minute auto-pause went, at 4 hours.
		IdleTimeout:      0,
		HibernateTimeout: 4 * time.Hour,
		HardTTL:          24 * time.Hour,
		// Persistent workstations have no TTL; 30 days matches the pricing page's promise.
		AbandonAfter: 30 * 24 * time.Hour,
		Logger:       daemonLogger(dashLogs),
		// The cache cannot see which files a Firecracker process has open.
		PinTemplate: pinTemplate,
		// What a guest is told to call as itself. The fleet's public control
		// plane is the default; PILLDROPS_API_URL overrides (a devserver, or a
		// portal on a different host than the one nodes heartbeat to).
		IdentityAPIURL: nodeboot.Env("PILLDROPS_API_URL", nodeboot.Env("ORCHESTRATOR_URL", "")),
		SandboxDomain:  nodeboot.Env("SANDBOXD_SANDBOX_DOMAIN", ""),
	})

	// Tracing is a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
	traceShutdown, err := observability.Init(bootCtx, observability.TraceConfigFromEnv("sandboxd",
		observability.TraceSampleRatio()))
	if err != nil {
		// Losing traces must never take the service down.
		slog.Warn("tracing disabled", "component", "trace", "err", err)
	}
	defer observability.Flush(traceShutdown)

	if err := config.Validate(); err != nil {
		return fmt.Errorf("configuration refused: %w", err)
	}
	config.LogResolved()

	// The daemon-level guard covers every caller, including the preview proxy's
	// wake path; api.SetIsolationOK below only covers create and resume requests.
	d.SetIsolationOK(!preflightFailed)
	d.Start()
	defer d.Stop()

	nodeboot.BootstrapKey(bootCtx, st, dataDir)

	// The dashboard session key is written to the data dir rather than printed
	// so it does not end up in shipped logs.
	_, sessionKey, err := st.CreateKey(bootCtx, "dashboard-session", "default")
	if err != nil {
		return fmt.Errorf("session key: %w", err)
	}
	sessionKeyPath := filepath.Join(dataDir, "dashboard-session.key")
	if err := os.WriteFile(sessionKeyPath, []byte(sessionKey+"\n"), 0o600); err != nil {
		slog.Error("could not write the dashboard session key; sign-in is unavailable",
			"component", "dashboard", "path", sessionKeyPath, "err", err)
	}

	sandboxDomain := nodeboot.Env("SANDBOXD_SANDBOX_DOMAIN", "")
	ui, err := webui.New(sessionKey, runtime.Name(), "user", orchURL != "", sandboxDomain)
	if err != nil {
		return fmt.Errorf("web ui: %w", err)
	}
	// The dashboard signs in with an account: the first visitor creates the
	// administrator, stored in this node's own database.
	ui.SetAdminStore(store.AccountsOf(st))

	mux := http.NewServeMux()
	api := nodeboot.NewNodeAPI(bootCtx, d, st, dataDir, sandboxDomain, orchURL, nodeboot.Env("ORCHESTRATOR_API_KEY", ""))
	// A node hit directly, not via the orchestrator, must also fail closed.
	api.SetIsolationOK(!preflightFailed)
	api.SetConsoleKey(sessionKey)
	api.Register(mux)
	ma, rps, burst := api.Limits()
	dash := dashboard.New(d, st, dashLogs, dashboard.Limits{MaxActive: ma, RateRPS: rps, RateBurst: burst})
	dash.SetConsoleKey(sessionKey)
	dash.Register(mux)
	ui.Register(mux)

	srv := &http.Server{
		Addr: addr,
		// The subdomain-form preview shares the org-bound token check with the /preview/ mount.
		Handler:     proxy.HostRouter(sandboxDomain, d, mux, api.VerifyPreview),
		IdleTimeout: 60 * time.Second,
		// Not ReadTimeout, which would also bound legitimately large upload bodies.
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the dashboard's SSE streams are long-lived.
	}
	// SSE streams never return on their own and would hold the drain until its timeout.
	srv.RegisterOnShutdown(dash.Close)

	// A firecracker node advertises only the templates on its disk; docker and
	// process advertise nothing, meaning unrestricted.
	bootable := nodeboot.LocalTemplates(runtime.Name(), templates.PublishedIDs)
	// Suppressing the heartbeat is what de-advertises a failed node to the scheduler.
	if !preflightFailed {
		nodeboot.StartHeartbeat(bootCtx, d, dataDir, nodeboot.Env("NODE_URL", nodeboot.LocalURL(addr)), runtime.Name(), isolation, bootable, artifactCanFetch)
	} else {
		slog.Warn("heartbeat suppressed: this node will not be scheduled by the orchestrator",
			"component", "preflight")
	}

	serveErr := make(chan error, 1)
	go func() {
		fmt.Printf("\n  sandboxd — runtime=%s isolation=%v version=%s\n", runtime.Name(), isolation, nodeboot.Version)
		fmt.Printf("  Dashboard: %s/dashboard  (key: %s)\n", nodeboot.LocalURL(addr), sessionKeyPath)
		fmt.Printf("  Docs:      %s/docs\n", nodeboot.LocalURL(addr))
		fmt.Printf("  API:       %s/v1/sandboxes\n", nodeboot.LocalURL(addr))
		fmt.Printf("  Health:    %s/health\n\n", nodeboot.LocalURL(addr))
		// nodeboot.Serve answers TLS and plaintext on the same socket.
		if err := nodeboot.Serve(srv, dataDir); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	// A listen failure travels back here so the defers above still run.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	var exitErr error
	select {
	case <-stop:
	case exitErr = <-serveErr:
		slog.Error("http server stopped", "err", exitErr)
	}
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	// Pause running sandboxes so the next boot restores them.
	d.Shutdown(ctx)
	return exitErr
}

// daemonLogger builds the daemon's logger with the dashboard's live pane teed off it.
func daemonLogger(dash *dashboard.LogBroadcaster) *slog.Logger {
	base := observability.NewLogger(os.Stderr, nodeboot.Env("LOG_FORMAT", "json"), nodeboot.Env("LOG_LEVEL", "info"))
	if dash == nil {
		return base.With("component", "daemon")
	}
	return slog.New(&observability.TeeHandler{
		Handler: base.Handler(),
		Publish: dash.Publish,
	}).With("component", "daemon")
}
