package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// imageBuildResponse is the POST /v1/images/build response.
type imageBuildResponse struct {
	// OK means built and, where the fleet has more than one node, published.
	OK  bool   `json:"ok"`
	Tag string `json:"tag"`
	Log string `json:"log,omitempty"`

	Build   imageBuildStep   `json:"build"`
	Publish imagePublishStep `json:"publish"`

	// Deprecated: read Build instead. Removed in the release after next.
	Nodes []legacyNodeResult `json:"nodes"`
}

// imageBuildStep is the one build that ran.
type imageBuildStep struct {
	Node string `json:"node"`
	OK   bool   `json:"ok"`
	Err  string `json:"error,omitempty"`
	// Payload is "image.tar" or "rootfs"; the two are launched differently.
	Payload string `json:"payload,omitempty"`
}

// imagePublishStep reports whether the fleet, not just the building node, can run the image.
type imagePublishStep struct {
	// Attempted is false when there is no artifact store or database to publish to.
	Attempted bool   `json:"attempted"`
	OK        bool   `json:"ok"`
	BuildID   string `json:"build_id,omitempty"`
	// TemplateID is the tag without its version suffix.
	TemplateID string `json:"template_id,omitempty"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Store      string `json:"store,omitempty"`
	Err        string `json:"error,omitempty"`
	// Payload is the artifact name the objects were written under.
	Payload string `json:"payload,omitempty"`
}

// legacyNodeResult is the old per-node element, kept field-for-field for old clients.
type legacyNodeResult struct {
	Node string `json:"node"`
	OK   bool   `json:"ok"`
	Err  string `json:"error,omitempty"`
}

// writeImageBuild fills in the deprecated array and decides the top-level ok.
// An unpublished build is fine on a fleet of one and a failure on more, since
// placement could send the sandbox to a node without the image.
func (s *Server) writeImageBuild(w http.ResponseWriter, resp imageBuildResponse, healthyNodes int) {
	resp.Nodes = []legacyNodeResult{{
		Node: resp.Build.Node, OK: resp.Build.OK, Err: resp.Build.Err,
	}}
	distributionNeeded := healthyNodes > 1
	resp.OK = resp.Build.OK && (!distributionNeeded || resp.Publish.OK)
	if resp.Build.OK && distributionNeeded && !resp.Publish.OK && resp.Publish.Err == "" {
		resp.Publish.Err = fmt.Sprintf(
			"the image was built on %s but not published, and this fleet has %d healthy nodes — "+
				"a sandbox scheduled anywhere else could not run it",
			resp.Build.Node, healthyNodes)
	}
	writeJSON(w, 200, resp)
}

// publishBuiltImage pulls the built image off the node and publishes it to the
// shared artifact store. The node cannot publish itself: node credentials are
// read-only on the store so a compromised node cannot replace a shared rootfs.
// Failures are recorded in out, never fatal.
func (s *Server) publishBuiltImage(r *http.Request, node *cluster.Node, built nodeBuildResult, org string, out *imagePublishStep) {
	st := s.st.Load()
	blobs := s.artifacts.Load()
	switch {
	case st == nil:
		out.Err = "the control plane's database connection is not up, so no build can be recorded"
		return
	case blobs == nil || *blobs == nil:
		out.Err = "no artifact store is configured on this control plane (set ORCHESTRATOR_ARTIFACT_DIR), " +
			"so a built image cannot be shared with the rest of the fleet"
		return
	}

	tag := built.Tag
	templateID, ok := artifact.TemplateIDForImageTag(tag)
	if !ok {
		out.Err = fmt.Sprintf("the node built tag %q, which does not reduce to a usable "+
			"template id, so the build cannot be indexed", tag)
		return
	}
	// The payload name must describe the bytes, so it comes from the node. An
	// older node that reports none can only have built an image tarball.
	payload := strings.TrimSpace(built.Payload)
	if payload == "" {
		payload = artifact.NameImage
	}
	if payload != artifact.NameImage && payload != artifact.NameRootfs {
		out.Err = fmt.Sprintf("node %s reported payload %q, which is neither %q nor %q",
			node.ID, payload, artifact.NameImage, artifact.NameRootfs)
		return
	}
	out.Attempted = true
	out.TemplateID = templateID
	out.Payload = payload

	// Detached from the caller: a client hanging up mid-stream must not waste
	// the build and leave the image on one node.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), imagePublishTimeout)
	defer cancel()

	body, err := s.exportImage(ctx, node, tag, payload)
	if err != nil {
		out.Err = err.Error()
		return
	}
	defer body.Close()

	buildID := newBuildID()
	res, err := publishBuild(ctx, st, *blobs, publishSpec{
		TemplateID: templateID,
		BuildID:    buildID,
		OrgID:      org,
		Payload:    payload,
		Body:       body,
		// docker save streams, so the length is unknown until it ends.
		Size: -1,
		// Empty for an image tarball, where envd is bind-mounted rather than baked in.
		EnvdVersion: built.EnvdSHA256,
		Now:         s.now().Unix(),
	})
	if err != nil {
		out.Err = err.Error()
		return
	}
	out.OK = true
	out.BuildID = res.Build.BuildID
	out.Size = res.Build.RootfsSize
	out.SHA256 = res.Build.RootfsSHA256
	out.Store = (*blobs).Name()
	slog.Info("published a build to the shared artifact store", "component", "artifact",
		"tag", tag, "template", templateID, "build", res.Build.BuildID, "payload", payload,
		"built_on", node.ID, "bytes", res.Build.RootfsSize)
}

// imagePublishTimeout bounds the export-and-publish leg for multi-gigabyte images.
const imagePublishTimeout = 30 * time.Minute

// exportImage streams the build's data object (docker save or the squashfs) from the node.
func (s *Server) exportImage(ctx context.Context, node *cluster.Node, tag, payload string) (io.ReadCloser, error) {
	// Streaming hop: no overall client timeout, ctx bounds the transfer.
	base, hop, err := s.nodeHop(node, hopStream)
	if err != nil {
		return nil, fmt.Errorf("export %s from %s: %w", tag, node.ID, err)
	}
	u := base + "/v1/images/export?tag=" + url.QueryEscape(tag)
	if payload == artifact.NameRootfs {
		u += "&payload=" + url.QueryEscape(payload)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("export %s from %s: %w", tag, node.ID, err)
	}
	if node.Key != "" {
		req.Header.Set("Authorization", "Bearer "+node.Key)
	}
	resp, err := hop.Do(req)
	if err != nil {
		return nil, fmt.Errorf("export %s from %s: %w", tag, node.ID, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("export %s from %s: HTTP %d %s",
			tag, node.ID, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}
