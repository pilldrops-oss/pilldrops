package nodeboot

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// ArtifactStoreConfig is the resolved artifact store choice, kept separate from
// construction so the decision is testable without a filesystem or network.
type ArtifactStoreConfig struct {
	Kind artifact.Kind
	Dir  string
	// BaseURL and Credential are empty on a standalone node.
	BaseURL    string
	Credential string
	// Clustered is the mode the decision was made under.
	Clustered bool
	// Explicit reports that an operator named the kind rather than it being defaulted.
	Explicit bool
	// S3 is read from SANDBOXD_ARTIFACT_S3_* and used only by KindS3.
	S3 artifact.S3Options
}

// ResolveArtifactStore reads SANDBOXD_ARTIFACT_STORE and SANDBOXD_ARTIFACT_DIR.
// The default kind is fs when standalone and controlplane when clustered.
func ResolveArtifactStore(dataDir, orchURL, nodeKey string) ArtifactStoreConfig {
	clustered := strings.TrimSpace(orchURL) != ""

	def := string(artifact.KindFS)
	if clustered {
		def = string(artifact.KindControlPlane)
	}
	raw := strings.ToLower(strings.TrimSpace(Env("SANDBOXD_ARTIFACT_STORE", def)))

	dir := Env("SANDBOXD_ARTIFACT_DIR", "")
	if dir == "" {
		dir = filepath.Join(dataDir, "artifacts")
	}

	return ArtifactStoreConfig{
		Kind:       artifact.Kind(raw),
		Dir:        dir,
		BaseURL:    strings.TrimRight(orchURL, "/"),
		Credential: nodeKey,
		Clustered:  clustered,
		Explicit:   raw != def,
		S3:         artifact.S3OptionsFromEnv("SANDBOXD_ARTIFACT_S3_", func(k string) string { return Env(k, "") }),
	}
}

// ArtifactStoreGuardError refuses a clustered node whose artifact store is not
// shared between nodes. A machine-local store makes creates succeed or fail
// depending on placement, which reads as a scheduler bug.
func ArtifactStoreGuardError(cfg ArtifactStoreConfig) error {
	if !cfg.Clustered || artifact.Shared(cfg.Kind) {
		return nil
	}
	how := "defaulted to"
	if cfg.Explicit {
		how = "configured as"
	}
	return fmt.Errorf(
		"this node is clustered (ORCHESTRATOR_URL is set) but its artifact store is %s %q, "+
			"which is local to this machine — a template published here would be invisible to every "+
			"other node, so creates would succeed or fail depending on which node the scheduler "+
			"picked. Set SANDBOXD_ARTIFACT_STORE to one of: %s. The %q store needs no further "+
			"configuration: it reuses ORCHESTRATOR_URL and this node's own key",
		how, cfg.Kind, strings.Join(sharedKinds(), ", "), artifact.KindControlPlane)
}

// s3Setup is the block of variables an operator has to set to get backups that
// survive the node. Written once so the boot warning and the 501 agree.
// Spelled out rather than abbreviated to a common prefix: this is the line an
// operator copies.
const s3Setup = "SANDBOXD_ARTIFACT_STORE=s3 with SANDBOXD_ARTIFACT_S3_ENDPOINT, " +
	"SANDBOXD_ARTIFACT_S3_BUCKET, SANDBOXD_ARTIFACT_S3_REGION, " +
	"SANDBOXD_ARTIFACT_S3_ACCESS_KEY and SANDBOXD_ARTIFACT_S3_SECRET_KEY"

// BackupStoreWarning says what this node's artifact store means for sandbox
// backups, or "" when a backup survives losing this machine. Nothing else
// tells an operator: the store is chosen at boot and the consequence only
// shows up the first time somebody asks for a backup, which on a default
// clustered node is a 501 and on a default standalone node is a copy of the
// disk sitting next to the disk.
func BackupStoreWarning(cfg ArtifactStoreConfig) string {
	switch cfg.Kind {
	case artifact.KindS3:
		return ""
	case artifact.KindFS:
		return "backups are written to this node's own disk (" + cfg.Dir + "), so they are lost " +
			"with the machine they exist to survive. For off-node backups set " + s3Setup
	default:
		return "backups are OFF on this node: every backup request answers 501 " +
			"backups_need_object_store, and any backup schedule on a sandbox here fails every " +
			"fire. The " + string(artifact.KindControlPlane) + " store cannot hold them — nodes " +
			"mount it read-only, so a PUT is refused. Set " + s3Setup
	}
}

// sharedKinds is derived from artifact.Kinds so the error message never
// recommends a backend the guard then refuses.
func sharedKinds() []string {
	var out []string
	for _, k := range artifact.Kinds() {
		if artifact.Shared(artifact.Kind(k)) {
			out = append(out, k)
		}
	}
	return out
}

// OpenArtifactStore resolves, guards and constructs in one call so no caller
// can skip the guard.
func OpenArtifactStore(dataDir, orchURL, nodeKey string, client *http.Client) (artifact.StorageProvider, ArtifactStoreConfig, error) {
	cfg := ResolveArtifactStore(dataDir, orchURL, nodeKey)
	if err := ArtifactStoreGuardError(cfg); err != nil {
		return nil, cfg, err
	}
	if cfg.Kind == artifact.KindControlPlane && cfg.Credential == "" {
		return nil, cfg, errors.New(
			"the control-plane artifact store needs this node's API key, and none was resolved — " +
				"a clustered node gets one from NODE_API_KEY or by redeeming a join token")
	}
	store, err := artifact.Open(cfg.Kind, artifact.Options{
		Root:       cfg.Dir,
		BaseURL:    cfg.BaseURL,
		Credential: cfg.Credential,
		HTTPClient: client,
		S3:         cfg.S3,
	})
	if err != nil {
		return nil, cfg, err
	}
	return store, cfg, nil
}
