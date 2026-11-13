package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// imageConfig is the part of an image's config its volume-free copy is rebuilt with.
type imageConfig struct {
	Env          []string
	Entrypoint   []string
	Cmd          []string
	WorkingDir   string
	User         string
	ExposedPorts map[string]struct{}
	Volumes      map[string]struct{}
}

// volumelessImage returns img, or a local copy of it that declares no volumes.
// `docker commit` leaves a volume out, so a database image's data directory
// was in no pause snapshot, checkpoint or backup, and every re-boot started on
// an empty one. Docker has no switch to ignore an image's volumes; a flattened
// copy keeps the data in the container's own layer, under the disk cap too.
func (r *Runtime) volumelessImage(ctx context.Context, img string) (string, error) {
	out, se, code, err := dockerOut(ctx, "image", "inspect", "-f", "{{.Id}} {{json .Config}}", img)
	if err != nil || code != 0 {
		return "", dockerErr("inspect "+img, err, code, se)
	}
	id, cfgJSON, _ := strings.Cut(strings.TrimSpace(out), " ")
	var cfg imageConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		return "", fmt.Errorf("read the config of image %s: %w", img, err)
	}
	if len(cfg.Volumes) == 0 {
		return img, nil
	}
	// Named by image id, so a tag that moves to a new image gets a new copy.
	// ponytail: copies are never removed. A capture taken before this fix gets
	// one on its first re-boot; prune sandboxd-novol if those pile up.
	tag := "sandboxd-novol:" + strings.TrimPrefix(id, "sha256:")
	// ponytail: one lock for every image; a per-image lock if first boots of
	// different service images ever queue behind each other.
	r.volumelessMu.Lock()
	defer r.volumelessMu.Unlock()
	if _, _, code, err := dockerOut(ctx, "image", "inspect", tag); err != nil || code != 0 {
		if err := flattenImage(ctx, img, tag, cfg); err != nil {
			return "", err
		}
	}
	return tag, nil
}

// flattenImage imports img's filesystem as tag with cfg's settings and no volumes.
func flattenImage(ctx context.Context, img, tag string, cfg imageConfig) error {
	changes, err := configChanges(cfg)
	if err != nil {
		return fmt.Errorf("image %s: %w", img, err)
	}
	out, se, code, err := dockerOut(ctx, "create", img)
	if err != nil || code != 0 {
		return dockerErr("create from "+img, err, code, se)
	}
	cid := strings.TrimSpace(out)
	defer func() { _, _, _, _ = dockerOut(context.WithoutCancel(ctx), "rm", "-v", cid) }()
	f, err := os.CreateTemp("", "sandboxd-novol-*.tar")
	if err != nil {
		return err
	}
	_ = f.Close()
	defer func() { _ = os.Remove(f.Name()) }()
	if _, se, code, err := dockerOut(ctx, "export", "-o", f.Name(), cid); err != nil || code != 0 {
		return dockerErr("export "+img, err, code, se)
	}
	args := []string{"import"}
	for _, c := range changes {
		args = append(args, "--change", c)
	}
	if _, se, code, err := dockerOut(ctx, append(args, f.Name(), tag)...); err != nil || code != 0 {
		return dockerErr("import "+tag, err, code, se)
	}
	return nil
}

var dockerfileQuoter = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)

// configChanges renders cfg as the Dockerfile instructions `docker import
// --change` takes. Volumes are the one thing left out.
func configChanges(cfg imageConfig) ([]string, error) {
	var ch []string
	for _, e := range cfg.Env {
		k, v, _ := strings.Cut(e, "=")
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("env %s holds a line break, which an imported image cannot carry", k)
		}
		ch = append(ch, "ENV "+k+`="`+dockerfileQuoter.Replace(v)+`"`)
	}
	if cfg.Entrypoint != nil {
		j, _ := json.Marshal(cfg.Entrypoint)
		ch = append(ch, "ENTRYPOINT "+string(j))
	}
	if cfg.Cmd != nil {
		j, _ := json.Marshal(cfg.Cmd)
		ch = append(ch, "CMD "+string(j))
	}
	if cfg.WorkingDir != "" {
		ch = append(ch, "WORKDIR "+cfg.WorkingDir)
	}
	if cfg.User != "" {
		ch = append(ch, "USER "+cfg.User)
	}
	for _, p := range slices.Sorted(maps.Keys(cfg.ExposedPorts)) {
		ch = append(ch, "EXPOSE "+p)
	}
	return ch, nil
}
