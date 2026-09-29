package nodeboot

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// ErrWrongPayload means the build this template resolves to holds the other
// runtime's artifact. Distinct from not-found because nothing is missing.
var ErrWrongPayload = errors.New("nodeboot: the published build is for a different runtime")

// buildMetadata reads a build's metadata.json, or reports ok=false. Not an
// error: builds published before custom images carry none, and the callers all
// have a sane default for that case.
func buildMetadata(ctx context.Context, remote artifact.StorageProvider, buildID string) (artifact.Metadata, bool) {
	if remote == nil {
		return artifact.Metadata{}, false
	}
	key, err := artifact.Key(buildID, artifact.NameMetadata)
	if err != nil {
		return artifact.Metadata{}, false
	}
	rc, err := remote.Get(ctx, key)
	if err != nil {
		return artifact.Metadata{}, false
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, artifact.MaxMetadataBytes+1))
	if err != nil {
		return artifact.Metadata{}, false
	}
	meta, err := artifact.ParseMetadata(raw)
	if err != nil {
		return artifact.Metadata{}, false
	}
	return meta, true
}

// checkPayload refuses a build whose metadata.json payload is not want, since
// a Firecracker template and a Docker image share a template id. A missing or
// unparseable metadata.json is accepted: older builds have none.
func checkPayload(ctx context.Context, remote artifact.StorageProvider, buildID, want string) error {
	meta, ok := buildMetadata(ctx, remote, buildID)
	if !ok {
		return nil
	}
	if got := meta.PayloadName(); got != want {
		return fmt.Errorf("%w: build %s holds %q, and this node needs %q. "+
			"A Firecracker template and a Docker custom image built from the same name share "+
			"one template id, so a fleet can publish either under it; only the runtime that "+
			"matches can boot the result", ErrWrongPayload, buildID, got, want)
	}
	return nil
}
