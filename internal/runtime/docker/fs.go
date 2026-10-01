package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// dockerFS implements rt.FileSystem over a container via docker exec.
//
// Paths are passed as argv or as "$1" to a script, never interpolated into
// shell text; Go's %q is not shell quoting. Absolute paths pass through
// (exec already reaches any container path), relative ones resolve under
// containerWorkdir, matching the envd contract.
type dockerFS struct {
	cid string
	// eng is the Engine API client, or nil to use the CLI.
	eng *engine
	// user is the sandbox run user, "" for root. The file API is a tenant
	// surface, so it runs as the same uid their shell does.
	user string
}

// execArgs prefixes a `docker exec` argv with the run user.
func (d *dockerFS) execArgs(rest ...string) []string {
	args := []string{"exec"}
	if d.user != "" {
		args = append(args, "--user", d.user)
	}
	return append(append(args, d.cid), rest...)
}

func (d *dockerFS) abs(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(containerWorkdir, path.Clean("/"+p))
}

// fsOpTimeout caps one filesystem operation; a sooner caller deadline wins.
const fsOpTimeout = 30 * time.Second

func opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= fsOpTimeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, fsOpTimeout)
}

// exec runs argv in the container with no shell involved.
func (d *dockerFS) exec(ctx context.Context, args ...string) (string, string, int, error) {
	ctx, cancel := opCtx(ctx)
	defer cancel()
	if d.eng != nil {
		so, se, code, err := d.eng.Exec(ctx, d.cid, execConfig{Cmd: args, User: d.user}, nil)
		if err == nil {
			return so, se, code, nil
		}
		// Only a transport failure falls back to the CLI; a non-zero exit is
		// not retried because it could repeat a side effect.
	}
	return dockerOut(ctx, d.execArgs(args...)...)
}

// shell runs a POSIX shell script in the container with args bound to $1, $2, ...
// The extra "sh" fills $0; without it the first path would become the shell's name.
func (d *dockerFS) shell(ctx context.Context, script string, args ...string) (string, string, int, error) {
	ctx, cancel := opCtx(ctx)
	defer cancel()
	argv := append([]string{"/bin/sh", "-c", script, "sh"}, args...)
	if d.eng != nil {
		so, se, code, err := d.eng.Exec(ctx, d.cid, execConfig{Cmd: argv, User: d.user}, nil)
		if err == nil {
			return so, se, code, nil
		}
	}
	return dockerOut(ctx, d.execArgs(argv...)...)
}

func (d *dockerFS) Read(ctx context.Context, p string) ([]byte, error) {
	out, se, code, err := d.exec(ctx, "cat", "--", d.abs(p))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if code != 0 {
		// The HTTP layer maps fs.ErrNotExist to 404, anything else to 503.
		if strings.Contains(se, "No such file") {
			return nil, fs.ErrNotExist
		}
		return nil, fsFail("read "+p, nil, se)
	}
	return []byte(out), nil
}

// writeScript creates the parent directory and streams stdin into $1.
const writeScript = `mkdir -p "$(dirname "$1")" && cat > "$1"`

func (d *dockerFS) Write(ctx context.Context, p string, data []byte) error {
	ctx, cancel := opCtx(ctx)
	defer cancel()
	if d.eng != nil {
		// Needs stdin with a half-close, or cat never sees EOF and the exec hangs.
		_, se, code, err := d.eng.Exec(ctx, d.cid,
			execConfig{Cmd: []string{"/bin/sh", "-c", writeScript, "sh", d.abs(p)}, User: d.user},
			bytes.NewReader(data))
		if err == nil {
			if code != 0 {
				return fsFail("write "+p, nil, se)
			}
			return nil
		}
	}
	cliArgs := []string{"exec", "-i"}
	if d.user != "" {
		cliArgs = append(cliArgs, "--user", d.user)
	}
	cliArgs = append(cliArgs, d.cid, "/bin/sh", "-c", writeScript, "sh", d.abs(p))
	cmd := exec.CommandContext(ctx, "docker", cliArgs...)
	cmd.Stdin = bytes.NewReader(data)
	var se bytes.Buffer
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		return fsFail("write "+p, err, se.String())
	}
	return nil
}

// WriteFrom implements rt.StreamFS: the request body is cat's stdin, so the
// node never holds the file. Falls back to the buffered CLI path without an engine.
func (d *dockerFS) WriteFrom(ctx context.Context, p string, r io.Reader) (int64, error) {
	if d.eng == nil {
		data, err := io.ReadAll(r)
		if err != nil {
			return 0, err
		}
		return int64(len(data)), d.Write(ctx, p, data)
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	cr := &countingReader{r: r}
	_, se, code, err := d.eng.Exec(ctx, d.cid,
		execConfig{Cmd: []string{"/bin/sh", "-c", writeScript, "sh", d.abs(p)}, User: d.user}, cr)
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fsFail("write "+p, nil, se)
	}
	return cr.n, nil
}

// ReadTo implements rt.StreamFS. ponytail: the Engine exec API returns stdout
// as one string, so a read is buffered on the node; stream it when engine.Exec
// grows a writer argument.
func (d *dockerFS) ReadTo(ctx context.Context, p string, w io.Writer) error {
	data, err := d.Read(ctx, p)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

// listScript prints one line per entry: name\ttype(d|f)\tsize\tmtimeEpoch.
// POSIX sh only, so it works on every slim base image.
const listScript = `cd "$1" || exit 1
for e in * .*; do
  [ -e "$e" ] || continue
  case "$e" in .|..) continue;; esac
  if [ -d "$e" ]; then t=d; sz=0; else t=f; sz=$(wc -c < "$e" 2>/dev/null | tr -d ' '); fi
  m=$(stat -c %Y "$e" 2>/dev/null || echo 0)
  printf '%s\t%s\t%s\t%s\n' "$e" "$t" "${sz:-0}" "$m"
done`

func (d *dockerFS) List(ctx context.Context, p string) ([]rt.DirEntry, error) {
	out, se, code, err := d.shell(ctx, listScript, d.abs(p))
	if err != nil || code != 0 {
		return nil, fsFail("list "+p, err, se)
	}
	var entries []rt.DirEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 4 {
			continue
		}
		// listScript already defaults size/mtime to 0; a parse failure is equally benign.
		size, _ := strconv.ParseInt(f[2], 10, 64) //nolint:errcheck // best-effort; falls back to 0
		mt, _ := strconv.ParseInt(f[3], 10, 64)   //nolint:errcheck // best-effort; falls back to 0
		entries = append(entries, rt.DirEntry{
			Name: f[0], IsDir: f[1] == "d", Size: size, Modified: time.Unix(mt, 0),
		})
	}
	return entries, nil
}

func (d *dockerFS) Mkdir(ctx context.Context, p string) error {
	_, se, code, err := d.exec(ctx, "mkdir", "-p", "--", d.abs(p))
	if err != nil || code != 0 {
		return fsFail("mkdir "+p, err, se)
	}
	return nil
}

// moveScript creates the destination's parent then moves $1 to $2.
const moveScript = `mkdir -p "$(dirname "$2")" && mv "$1" "$2"`

func (d *dockerFS) Move(ctx context.Context, src, dst string) error {
	_, se, code, err := d.shell(ctx, moveScript, d.abs(src), d.abs(dst))
	if err != nil || code != 0 {
		return fsFail("move "+src+"->"+dst, err, se)
	}
	return nil
}

func (d *dockerFS) Remove(ctx context.Context, p string) error {
	_, se, code, err := d.exec(ctx, "rm", "-rf", "--", d.abs(p))
	if err != nil || code != 0 {
		return fsFail("remove "+p, err, se)
	}
	return nil
}

func (d *dockerFS) Stat(ctx context.Context, p string) (rt.FileInfo, error) {
	out, se, code, err := d.exec(ctx, "stat", "-c", "%s|%F|%Y|%a", "--", d.abs(p))
	if err != nil {
		return rt.FileInfo{}, err
	}
	if code != 0 {
		if strings.Contains(se, "No such file") {
			return rt.FileInfo{}, fs.ErrNotExist
		}
		return rt.FileInfo{}, fsFail("stat "+p, nil, se)
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 4)
	if len(parts) < 3 {
		return rt.FileInfo{}, fmt.Errorf("stat %s: bad output %q", p, out)
	}
	size, _ := strconv.ParseInt(parts[0], 10, 64) //nolint:errcheck // best-effort; falls back to 0
	mt, _ := strconv.ParseInt(parts[2], 10, 64)   //nolint:errcheck // best-effort; falls back to 0
	mode := ""
	if len(parts) == 4 {
		mode = parts[3]
	}
	return rt.FileInfo{
		Name:     path.Base(p),
		IsDir:    strings.Contains(parts[1], "directory"),
		Size:     size,
		Mode:     mode,
		Modified: time.Unix(mt, 0),
	}, nil
}

// fsFail is a failed helper's error, tagged with the sentinel its stderr
// names so the API can answer 404/409/403/400 rather than 503. err is the
// exec's own failure, nil when the helper ran and exited non-zero.
func fsFail(what string, err error, stderr string) error {
	se := strings.TrimSpace(stderr)
	if kind := rt.FSKind(se); kind != nil {
		return fmt.Errorf("%w: %s: %s", kind, what, se)
	}
	if err != nil {
		return fmt.Errorf("%s: %w %s", what, err, se)
	}
	return fmt.Errorf("%s: %s", what, se)
}

var _ rt.FileSystem = (*dockerFS)(nil)
