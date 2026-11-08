// Command orchestrator is the multi-node control plane. Nodes heartbeat their
// capacity to it; it schedules each sandbox onto the best-fit node, proxies
// traffic to the owning node, and serves the dashboard SPA.
//
// Env:
//
//	PORT                  listen port (default 9090)
//	ORCHESTRATOR_ROLE     "edge" serves only sandbox traffic (previews, customer
//	                      domains) from the shared Redis; it runs no API,
//	                      dashboard, reconciler or background job.
//	                      Unset: the full control plane
//	ORCHESTRATOR_API_KEY  bearer required on client /v1 requests + the admin SPA's
//	                      session key (optional; unset = open)
//	ORCHESTRATOR_TOKEN    shared secret nodes present when they heartbeat (optional)
//	ORCHESTRATOR_ARTIFACT_DIR  where published template artifacts are stored;
//	                      unset means publishing is refused
//	MAX_ARTIFACT_BYTES    publish body ceiling (default 8 GiB)
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/observability"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/orchestrator"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/retention"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// stop is called explicitly because os.Exit skips defers; calling it twice is safe.
	err := run(ctx, stop)
	stop()
	if err != nil {
		slog.Error("refusing to start", "component", "orchestrator", "err", err)
		os.Exit(1)
	}
}

// run is the whole process. Boot guards return errors rather than calling
// os.Exit so tests can assert which misconfiguration is refused and defers run.
func run(ctx context.Context, stop func()) error {
	port := config.Str("PORT", "9090")

	// ORCHESTRATOR_TOKEN set is what makes this a fleet control plane; without
	// it no node can register, so the key guards do not apply.
	nodeKeySecret := os.Getenv("NODE_KEY_SECRET")
	clustered := os.Getenv("ORCHESTRATOR_TOKEN") != ""
	devInsecure := os.Getenv("DEV_INSECURE") == "1" || os.Getenv("SANDBOXD_DEV") == "1"
	if err := orchestrator.NodeKeySecretGuardError(clustered, devInsecure, nodeKeySecret); err != nil {
		return err
	}
	if err := orchestrator.OrgAssertKeyGuardError(clustered, devInsecure, os.Getenv("ORG_ASSERT_KEY")); err != nil {
		return err
	}
	if err := orchestrator.PortalOriginGuardError(os.Getenv("PORTAL_ORIGIN")); err != nil {
		return err
	}
	if _, err := proxy.ParseLegacyDomains(os.Getenv("PORTAL_LEGACY_SANDBOX_DOMAINS"),
		os.Getenv("PORTAL_SANDBOX_DOMAIN"), os.Getenv("PORTAL_ORIGIN")); err != nil {
		return err
	}
	if err := orchestrator.VaultKeyGuardError(clustered, devInsecure,
		os.Getenv("ALLOW_PLAINTEXT_VAULT") == "1", os.Getenv("VAULT_SECRET_KEY")); err != nil {
		return err
	}

	edge := os.Getenv("ORCHESTRATOR_ROLE") == "edge"
	if edge && os.Getenv("REDIS_URL") == "" {
		return errors.New("ORCHESTRATOR_ROLE=edge needs REDIS_URL: an edge replica routes from the " +
			"shared route table and would otherwise know no sandbox at all")
	}

	srv := orchestrator.NewServer(os.Getenv("ORCHESTRATOR_API_KEY"), os.Getenv("ORCHESTRATOR_TOKEN"))
	// Opened in the background so a slow DB never blocks /health at boot; until
	// it connects, admin handlers fall back to the node proxy.
	var db atomic.Pointer[store.Store]
	if dsn := os.Getenv("SANDBOXD_DATABASE_URL"); dsn != "" {
		go func() {
			if st := openStoreWithRetry(ctx, dsn, store.Open, storeOpenRetry); st != nil {
				// VAULT_SECRET_KEY is deliberately not in the node join bundle, so a
				// node cannot read a tenant's provider credential from the shared DB.
				// Nodes are handed NODE_BOOTSTRAP_SECRET as their key at join; without it here
				// every row a node encrypted fails to read, and move/rehome/routing answer 404.
				if err := st.SetSecretKey(cmp.Or(os.Getenv("SANDBOXD_SECRET_KEY"), os.Getenv("NODE_BOOTSTRAP_SECRET"))); err != nil {
					slog.Error("SANDBOXD_SECRET_KEY unusable", "component", "orchestrator", "err", err)
				}
				if err := st.SetVaultKey(os.Getenv("VAULT_SECRET_KEY")); err != nil {
					slog.Error("VAULT_SECRET_KEY unusable", "component", "orchestrator", "err", err)
				}
				if os.Getenv("VAULT_SECRET_KEY") == "" {
					slog.Warn("VAULT_SECRET_KEY is not set: connecting a provider key will be REFUSED "+
						"rather than stored unencrypted (set ALLOW_PLAINTEXT_VAULT=1 for local dev only)",
						"component", "orchestrator")
				}
				st.SetAllowPlaintextVault(os.Getenv("ALLOW_PLAINTEXT_VAULT") == "1")

				db.Store(st)
				srv.SetStore(st)
				if edge {
					// The edge reads rows (visibility, customer domains); the
					// control plane replicas own every background writer.
					return
				}
				// Customer hostnames route only once this has seen their DNS record.
				srv.StartDomainVerifier(ctx, st)
				// Shared root images nothing references any more.
				srv.StartRootfsGC(ctx)
				// A source-network restriction behind an untrusted proxy admits
				// everyone or nobody and looks like it works. Say so at boot.
				srv.WarnIfSourceGateCannotWork(ctx)
				slog.Info("admin analytics reading the shared Postgres directly", "component", "orchestrator")
				// Retention also runs on nodes; both pruning is harmless, and the
				// control plane is the one process always attached to this DB.
				retention.Start(ctx, st)
			}
		}()
	}

	// Without ORG_ASSERT_KEY the HMAC fallback signs with ORCHESTRATOR_TOKEN,
	// which is a per-node heartbeat token, so every relayed X-Org-Override
	// fails verification at the node.
	if os.Getenv("ORG_ASSERT_KEY") == "" {
		slog.Warn("ORG_ASSERT_KEY is not set. In a fleet this does NOT degrade to the old shared-secret "+
			"path — it BREAKS org overrides outright: the HMAC fallback signs with ORCHESTRATOR_TOKEN, "+
			"and since NODE_KEY_SECRET became mandatory a node's ORCHESTRATOR_TOKEN is its own heartbeat "+
			"token, not the fleet secret. Every tenant-scoped request will 403 at the node. "+
			"Generate one with `openssl rand -hex 32` and re-join nodes so they receive the public half.",
			"component", "orchestrator")
	}

	// Installed as the slog default so package-level helpers log through the
	// same handler, which stamps request_id and trace_id from the context.
	slog.SetDefault(observability.NewLogger(os.Stderr, os.Getenv("LOG_FORMAT"), os.Getenv("LOG_LEVEL")))

	// Tracing is a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
	traceShutdown, err := observability.Init(ctx, observability.TraceConfigFromEnv("orchestrator",
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

	// Unset means publishing is refused rather than written to ephemeral storage
	// that vanishes on the next deploy.
	if strings.EqualFold(os.Getenv("ORCHESTRATOR_ARTIFACT_STORE"), string(artifact.KindS3)) {
		// A bucket (S3, R2, MinIO, or GCS through its S3 API) is the store every
		// replica shares and every node can read directly, instead of one
		// replica's disk.
		blobs, err := artifact.Open(artifact.KindS3, artifact.Options{
			S3: artifact.S3OptionsFromEnv("ORCHESTRATOR_ARTIFACT_S3_", os.Getenv),
		})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err = blobs.List(ctx, "metadata-probe/")
			cancel()
		}
		if err != nil {
			return fmt.Errorf("ORCHESTRATOR_ARTIFACT_STORE=s3 is unusable; refusing to start rather than "+
				"serving a control plane that accepts publishes it cannot store: %w", err)
		}
		srv.SetArtifactStore(blobs)
		slog.Info("artifact store ready", "component", "artifact", "kind", blobs.Name(),
			"bucket", os.Getenv("ORCHESTRATOR_ARTIFACT_S3_BUCKET"))
	} else if dir := os.Getenv("ORCHESTRATOR_ARTIFACT_DIR"); dir != "" {
		blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: dir})
		if err != nil {
			return fmt.Errorf("ORCHESTRATOR_ARTIFACT_DIR %q is unusable; refusing to start rather than "+
				"serving a control plane that accepts publishes it cannot store: %w", dir, err)
		}
		srv.SetArtifactStore(blobs)
		slog.Info("artifact store ready", "component", "artifact", "kind", blobs.Name(), "dir", dir)
	} else {
		slog.Info("no artifact store configured: publishing a template artifact will be refused "+
			"(set ORCHESTRATOR_ARTIFACT_DIR)", "component", "artifact")
	}

	// The placement table is in-memory; rebuild it from the nodes so a restart recovers it.
	reconcilerStop := make(chan struct{})
	handler := srv.Routes()
	if edge {
		handler = srv.EdgeRoutes()
		slog.Info("edge role: serving sandbox traffic only", "component", "orchestrator")
	} else {
		srv.StartPlacementReconciler(10*time.Second, reconcilerStop)
	}

	// Raw public ports terminate on exactly one replica; every other replica
	// serves the forwards API and lets this one bind. No-op without FORWARD_INGRESS=1.
	srv.StartForwardIngress(ctx)

	// ReadHeaderTimeout closes the Slowloris hole on the internet-facing front
	// door. WriteTimeout is deliberately unset because this server relays SSE
	// and long-lived event streams.
	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// A listen failure travels back to run so the defers above still run.
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "component", "orchestrator", "port", port)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	var exitErr error
	select {
	case <-ctx.Done():
	case exitErr = <-serveErr:
		slog.Error("http server stopped", "component", "orchestrator", "err", exitErr)
	}
	stop() // restore default signal handling: a second Ctrl-C kills immediately
	slog.Info("shutting down", "component", "orchestrator")

	// Drain in-flight requests; every deploy sends SIGTERM.
	shutCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		slog.Error("graceful shutdown failed", "component", "orchestrator", "err", err)
	}
	close(reconcilerStop)
	if st := db.Load(); st != nil {
		if err := st.Close(); err != nil {
			slog.Error("closing the store failed", "component", "orchestrator", "err", err)
		}
	}
	return exitErr
}

const storeOpenRetry = 5 * time.Second

// openStoreWithRetry tries until the store opens or ctx ends, which returns nil.
// A database down at boot would otherwise need this process restarted by hand:
// it stays alive and unready, so nothing restarts it.
func openStoreWithRetry(ctx context.Context, dsn string, open func(context.Context, string) (*store.Store, error), wait time.Duration) *store.Store {
	for {
		st, err := open(ctx, dsn)
		if err == nil {
			return st
		}
		slog.Error("SANDBOXD_DATABASE_URL is set but the store could not be opened; retrying, and not ready until it does",
			"component", "orchestrator", "retry_in", wait.String(), "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
