package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/templatebuild"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// buildTimeout bounds a single image build.
const buildTimeout = 15 * time.Minute

// imageSlug lowercases and sanitizes s into a Docker-name-safe token (a-z0-9._-).
func imageSlug(s string) string { return templates.CustomImageSlug(s) }

// customImageTag is the org-scoped local tag for a custom image, so two orgs'
// "myapp" never collide.
func customImageTag(org, name string) string {
	return "sf-" + imageSlug(org) + "-" + imageSlug(name) + ":latest"
}

// handleImageBuild builds a tar[.gz] build context from the request body into
// one of two artifacts:
//
//	payload=image.tar  a Docker image tagged for this org
//	payload=rootfs     a Firecracker template (bootable squashfs via internal/templatebuild)
//
// One node builds; the control plane exports the result through
// handleImageExport and publishes it to the artifact store for the rest.
//
// POST /v1/images/build?name=<name>[&payload=rootfs|image.tar]
func (s *Server) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		writeErr(w, http.StatusBadRequest, "name query param required")
		return
	}
	payload, err := s.buildPayload(r.URL.Query().Get("payload"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	tag := customImageTag(orgID(r), name)
	// Must reduce the tag the same way the control plane and fetching node do,
	// or the template is published under one id and looked for under another.
	templateID, idOK := artifact.TemplateIDForImageTag(tag)

	// Preflight before spending fifteen minutes of docker build.
	if payload == artifact.NameRootfs {
		if !idOK {
			writeErr(w, http.StatusBadRequest,
				"tag "+tag+" does not reduce to a usable template id")
			return
		}
		if templates.Dir() == "" {
			writeErr(w, http.StatusBadRequest,
				"this node has no image directory (SNAPSHOT_DIR), so it cannot hold a "+
					"Firecracker template; ask for payload=image.tar or build on a node that can")
			return
		}
		if err := templatebuild.Preflight(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), buildTimeout)
	defer cancel()
	// "-" reads the context from stdin; gzip is auto-detected.
	cmd := exec.CommandContext(ctx, "docker", "build", "--pull", "-t", tag, "-")
	cmd.Stdin = r.Body
	out, err := cmd.CombinedOutput()
	log := string(out)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "tag": tag, "payload": payload, "error": err.Error(),
			"log": tailLog(log, 8000),
		})
		return
	}

	resp := map[string]any{
		"ok": true, "tag": tag, "payload": payload, "log": tailLog(log, 8000),
	}
	if payload == artifact.NameRootfs {
		res, err := templatebuild.Convert(ctx, templatebuild.Config{
			Tag: tag,
			// Written into the image directory so the template is bootable on
			// this node as soon as conversion finishes.
			Out: filepath.Join(templates.Dir(), templateID, "rootfs.squashfs"),
			// Scratch beside the output, not /tmp, which is routinely a small tmpfs.
			WorkDir: templates.Dir(),
		})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"ok": false, "tag": tag, "payload": payload, "template_id": templateID,
				"error": "converting the build context to a template rootfs: " + err.Error(),
				"log":   tailLog(log, 8000),
			})
			return
		}
		resp["template_id"] = templateID
		resp["rootfs_size"] = res.Size
		resp["envd_sha256"] = res.EnvdSHA256
		resp["convert_ms"] = res.Duration.Milliseconds()
		slog.Info("built a Firecracker template from a build context", "component", "artifact",
			"tag", tag, "template", templateID, "path", res.Path, "bytes", res.Size,
			"envd_sha256", res.EnvdSHA256, "convert_ms", res.Duration.Milliseconds())
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildPayload resolves the requested payload. Empty means whatever this node
// boots; an explicit request this node cannot satisfy is refused, never downgraded.
func (s *Server) buildPayload(want string) (string, error) {
	firecracker := s.d != nil && s.d.RuntimeName() == "firecracker"
	switch strings.TrimSpace(want) {
	case "":
		if firecracker {
			return artifact.NameRootfs, nil
		}
		return artifact.NameImage, nil
	case artifact.NameRootfs, "template", "firecracker":
		if !firecracker {
			return "", fmt.Errorf("this node runs the %q runtime, so it cannot build a "+
				"Firecracker template rootfs", s.d.RuntimeName())
		}
		return artifact.NameRootfs, nil
	case artifact.NameImage, "image", "docker":
		return artifact.NameImage, nil
	default:
		return "", fmt.Errorf("payload must be %q or %q", artifact.NameRootfs, artifact.NameImage)
	}
}

// exportTimeout bounds `docker save`.
const exportTimeout = 10 * time.Minute

// handleImageExport streams a locally-built image (docker save tarball, or the
// squashfs with payload=rootfs) so the control plane can publish it.
//
// GET /v1/images/export?tag=<tag>[&payload=rootfs|image.tar]
//
// Operator only: the caller supplies the tag, so a customer key here would be a
// cross-tenant read of other orgs' images.
func (s *Server) handleImageExport(w http.ResponseWriter, r *http.Request) {
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	if tag == "" {
		writeErr(w, http.StatusBadRequest, "tag query param required")
		return
	}
	// The tag reaches argv: reject anything shaped like a flag or a path.
	if !safeImageTag(tag) {
		writeErr(w, http.StatusBadRequest,
			"tag may contain only letters, digits, '.', '_', '-', ':' and '/', and may not begin with '-'")
		return
	}

	if strings.TrimSpace(r.URL.Query().Get("payload")) == artifact.NameRootfs {
		s.exportTemplateRootfs(w, tag)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
	defer cancel()
	// Probe first so an unknown tag is a 404 rather than a 200 with an error body.
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run(); err != nil {
		writeErr(w, http.StatusNotFound, "no image tagged "+tag+" on this node")
		return
	}

	cmd := exec.CommandContext(ctx, "docker", "save", tag)
	out, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "docker save: "+err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusInternalServerError, "docker save: "+err.Error())
		return
	}
	defer func() { _ = cmd.Wait() }()

	// No Content-Length: the size is unknown until docker save finishes.
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, out); err != nil {
		// The status is already sent; the caller sees a short read and verifies the hash.
		cancel()
		return
	}
}

// exportTemplateRootfs streams the squashfs a rootfs build left in the image
// directory, with Content-Length so a truncated transfer is a transport error.
func (s *Server) exportTemplateRootfs(w http.ResponseWriter, tag string) {
	dir := templates.Dir()
	if dir == "" {
		writeErr(w, http.StatusNotFound, "this node has no image directory, so it holds no templates")
		return
	}
	id, ok := artifact.TemplateIDForImageTag(tag)
	if !ok {
		writeErr(w, http.StatusBadRequest, "tag "+tag+" does not reduce to a usable template id")
		return
	}
	// id is constrained to [A-Za-z0-9._-] by TemplateIDForImageTag, so this cannot leave dir.
	path := filepath.Join(dir, id, "rootfs.squashfs")
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound,
			"no template rootfs for "+id+" on this node")
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stat template rootfs: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// safeImageTag reports whether tag is a Docker reference shape with nothing
// argv would treat as an option or a path escape.
func safeImageTag(tag string) bool {
	if tag == "" || len(tag) > 256 || strings.HasPrefix(tag, "-") {
		return false
	}
	if strings.Contains(tag, "..") {
		return false
	}
	for _, r := range tag {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' || r == ':' || r == '/'
		if !ok {
			return false
		}
	}
	return true
}

// tailLog keeps the last n bytes of a build log.
func tailLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
