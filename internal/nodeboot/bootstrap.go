package nodeboot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// BootstrapKey mints the first operator API key on first boot, tracked by a
// marker file. It is written to a 0600 file, not stdout, so log capture is not key capture.
func BootstrapKey(ctx context.Context, st *store.Store, dataDir string) {
	marker := filepath.Join(dataDir, ".bootstrapped")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	_, key, err := st.CreateKeyWithRole(ctx, "bootstrap", "default", "operator")
	if err != nil {
		slog.Error("could not create the node key", "component", "bootstrap", "err", err)
		return
	}
	keyPath := filepath.Join(dataDir, "bootstrap.key")
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		slog.Error("could not write the node key", "component", "bootstrap", "path", keyPath, "err", err)
		return
	}
	_ = os.WriteFile(marker, []byte("1"), 0o600)
	fmt.Printf("\n  first-run operator API key written to %s (chmod 0600)\n", keyPath)
	fmt.Printf("  read it once, store it in your secret manager, then delete the file.\n")
}

// LocalPreviewSecret returns this node's own preview-signing secret, minting it
// into dataDir on first use so preview links outlive a restart. Empty on error,
// which keeps preview gating closed.
func LocalPreviewSecret(dataDir string) string {
	path := filepath.Join(dataDir, "preview.secret")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 32 {
			return s
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		slog.Error("could not mint a preview secret; previews stay closed", "component", "bootstrap", "err", err)
		return ""
	}
	secret := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		slog.Error("could not persist the preview secret; previews stay closed",
			"component", "bootstrap", "path", path, "err", err)
		return ""
	}
	return secret
}
