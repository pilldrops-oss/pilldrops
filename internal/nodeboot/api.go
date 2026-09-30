package nodeboot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/extras/functions"
	"github.com/pilldrops-oss/pilldrops/internal/httpapi"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// NewNodeAPI builds the node HTTP API shared by cmd/sandboxd and cmd/sandboxd-fleet.
func NewNodeAPI(ctx context.Context, d *daemon.Daemon, st *store.Store, dataDir, sandboxDomain, orchURL, orchKey string) *httpapi.Server {
	api := httpapi.New(d, st)
	api.SetSandboxDomain(sandboxDomain)
	api.SetDataDir(dataDir)
	api.SetCluster(orchURL, orchKey)
	// X-Org-Override is signed with ORCHESTRATOR_TOKEN. Empty when unclustered,
	// so the node refuses any override unless DEV_INSECURE is set.
	api.SetOrgOverrideSecret([]byte(Env("ORCHESTRATOR_TOKEN", "")))
	// Must match the id the heartbeat registers, so an assertion signed for a
	// sibling node does not verify here.
	api.SetNodeID(NodeID())
	// An asymmetric key replaces HMAC verification. A parse failure is fatal
	// rather than a silent fallback to the forgeable HMAC form.
	if pubHex := Env("ORG_ASSERT_PUBLIC_KEY", ""); pubHex != "" {
		pub, err := orgassert.ParsePublicKey(pubHex)
		if err != nil {
			slog.Error("ORG_ASSERT_PUBLIC_KEY unusable; refusing to fall back to the forgeable HMAC form",
				"component", "node", "err", err)
			os.Exit(1)
		}
		api.SetOrgAssertPublicKey(pub)
	}
	// Preview gate secret, in precedence order. A standalone node mints its own,
	// so previews work with no configuration; a clustered node must share the
	// orchestrator's, which signs the tokens, so an unset one fails closed there.
	previewSecret := firstNonEmpty(
		Env("PREVIEW_SECRET", ""),
		Env("ORCHESTRATOR_TOKEN", ""),
		Env("SANDBOXD_SECRET_KEY", ""),
	)
	if previewSecret == "" && orchURL == "" {
		previewSecret = LocalPreviewSecret(dataDir)
	}
	api.SetPreviewSecret([]byte(previewSecret))
	// /v1/capabilities and the heartbeat must report the same local templates.
	api.SetLocalTemplates(LocalTemplates(d.RuntimeName(), templates.PublishedIDs))
	api.SetFunctions(functions.NewRegistry(d, st, filepath.Join(dataDir, "functions")))
	api.StartCronSweeper()
	// Agent features; no-op under -tags core until they are removed.
	startFeatures(api, d, st, dataDir)
	return api
}

// NodeID returns NODE_ID, falling back to the hostname.
func NodeID() string {
	id := Env("NODE_ID", "")
	if id == "" {
		id, _ = os.Hostname()
	}
	return id
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
