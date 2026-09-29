package nodeboot

import (
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

func TestResolveArtifactStoreDefaults(t *testing.T) {
	t.Setenv("SANDBOXD_ARTIFACT_STORE", "")
	t.Setenv("SANDBOXD_ARTIFACT_DIR", "")

	standalone := ResolveArtifactStore("/var/lib/sandbox/state", "", "")
	if standalone.Kind != artifact.KindFS {
		t.Errorf("standalone default = %q, want fs", standalone.Kind)
	}
	if standalone.Clustered {
		t.Error("no ORCHESTRATOR_URL means standalone")
	}
	if standalone.Dir != "/var/lib/sandbox/state/artifacts" {
		t.Errorf("dir = %q, want it under the data dir", standalone.Dir)
	}
	if standalone.Explicit {
		t.Error("a defaulted kind must not be reported as explicit")
	}

	clustered := ResolveArtifactStore("/var/lib/sandbox/state", "http://cp:9090", "nodekey")
	if clustered.Kind != artifact.KindControlPlane {
		t.Errorf("clustered default = %q, want controlplane", clustered.Kind)
	}
	if !clustered.Clustered {
		t.Error("ORCHESTRATOR_URL set means clustered")
	}
	if clustered.BaseURL != "http://cp:9090" {
		t.Errorf("base url = %q", clustered.BaseURL)
	}
}

func TestResolveArtifactStoreExplicit(t *testing.T) {
	t.Setenv("SANDBOXD_ARTIFACT_DIR", "")
	t.Setenv("SANDBOXD_ARTIFACT_STORE", "fs")

	cfg := ResolveArtifactStore("/data", "http://cp:9090", "k")
	if cfg.Kind != artifact.KindFS || !cfg.Explicit {
		t.Fatalf("kind=%q explicit=%v; want fs, explicit", cfg.Kind, cfg.Explicit)
	}
}

func TestArtifactStoreGuardError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     ArtifactStoreConfig
		wantErr bool
	}{
		{"standalone with fs is fine", ArtifactStoreConfig{Kind: artifact.KindFS}, false},
		{"standalone with controlplane is fine", ArtifactStoreConfig{Kind: artifact.KindControlPlane}, false},
		{"clustered with controlplane is fine", ArtifactStoreConfig{Kind: artifact.KindControlPlane, Clustered: true}, false},
		{"clustered with fs refuses", ArtifactStoreConfig{Kind: artifact.KindFS, Clustered: true}, true},
		// An unknown kind is not shared, so refuse it here rather than in Open.
		{"clustered with an unknown kind refuses", ArtifactStoreConfig{Kind: "nonsense", Clustered: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ArtifactStoreGuardError(tc.cfg)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestArtifactStoreGuardErrorRecommendsOnlyOpenableStores(t *testing.T) {
	t.Parallel()

	err := ArtifactStoreGuardError(ArtifactStoreConfig{Kind: artifact.KindFS, Clustered: true})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "SANDBOXD_ARTIFACT_STORE") {
		t.Error("the message must name the setting to change")
	}
	if !strings.Contains(msg, string(artifact.KindControlPlane)) {
		t.Error("the message must recommend the controlplane store")
	}
	for _, rec := range sharedKinds() {
		if _, openErr := artifact.Open(artifact.Kind(rec), artifact.Options{
			BaseURL: "http://cp", Credential: "k",
			S3: artifact.S3Options{Endpoint: "https://s3", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		}); openErr != nil {
			t.Errorf("the guard recommends %q but Open rejects it: %v", rec, openErr)
		}
	}
}

func TestOpenArtifactStoreNeedsACredentialWhenClustered(t *testing.T) {
	t.Setenv("SANDBOXD_ARTIFACT_STORE", "")
	t.Setenv("SANDBOXD_ARTIFACT_DIR", "")

	_, _, err := OpenArtifactStore(t.TempDir(), "http://cp:9090", "", nil)
	if err == nil {
		t.Fatal("want an error when clustered with no node key")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("error should name the missing credential, got: %v", err)
	}
}

func TestOpenArtifactStoreStandaloneWorksWithNoConfiguration(t *testing.T) {
	t.Setenv("SANDBOXD_ARTIFACT_STORE", "")
	t.Setenv("SANDBOXD_ARTIFACT_DIR", "")

	store, cfg, err := OpenArtifactStore(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("a single node must work with no artifact configuration at all: %v", err)
	}
	if store.Name() != "fs" || cfg.Kind != artifact.KindFS {
		t.Fatalf("store = %q, kind = %q; want fs", store.Name(), cfg.Kind)
	}
}

// The defect this guards: a node whose store cannot hold a backup off the
// machine said nothing at boot, and both of these are DEFAULTS — a clustered
// node answers 501 to every backup and a standalone node writes the backup
// beside the disk it copied. Driven through ResolveArtifactStore so changing
// either default without changing the warning fails here.
func TestBackupStoreWarningOnTheDefaults(t *testing.T) {
	t.Setenv("SANDBOXD_ARTIFACT_STORE", "")
	t.Setenv("SANDBOXD_ARTIFACT_DIR", "")

	clustered := BackupStoreWarning(ResolveArtifactStore("/data", "http://cp:9090", "k"))
	if !strings.Contains(clustered, "backups are OFF") {
		t.Errorf("a default clustered node takes no backups at all; warning = %q", clustered)
	}
	standalone := BackupStoreWarning(ResolveArtifactStore("/data", "", ""))
	if !strings.Contains(standalone, "/data/artifacts") {
		t.Errorf("a default standalone node backs up onto its own disk and the warning must "+
			"name where; warning = %q", standalone)
	}
	for _, w := range []string{clustered, standalone} {
		if !strings.Contains(w, "SANDBOXD_ARTIFACT_S3_SECRET_KEY") {
			t.Errorf("a warning with no remedy is noise; warning = %q", w)
		}
	}
	if w := BackupStoreWarning(ArtifactStoreConfig{Kind: artifact.KindS3}); w != "" {
		t.Errorf("s3 backups survive the node, so there is nothing to warn about; got %q", w)
	}
}

// The warning and the store that actually gets wired must not disagree: a
// store that cannot take backups must always say so.
func TestBackupStoreWarningAgreesWithBackupCapable(t *testing.T) {
	for _, k := range artifact.Kinds() {
		kind := artifact.Kind(k)
		if !backupCapableStore(kind) && BackupStoreWarning(ArtifactStoreConfig{Kind: kind}) == "" {
			t.Errorf("%q takes no backups and says nothing about it", k)
		}
	}
	// fs is the one that is both: it takes backups, and they die with the node.
	if !backupCapableStore(artifact.KindFS) ||
		BackupStoreWarning(ArtifactStoreConfig{Kind: artifact.KindFS, Dir: "/d"}) == "" {
		t.Error("fs must take backups AND warn that they share the node's fate")
	}
}
