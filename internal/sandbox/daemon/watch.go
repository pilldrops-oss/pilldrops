package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/envdproto"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// watchCmd runs envd's watcher on the directory in $PD_WATCH_DIR. The path is
// an env var, never shell text, so no quoting can let it become a command.
const watchCmd = `exec /usr/local/bin/envd watch "$PD_WATCH_DIR"`

// WatchDir streams file events for one directory as JSON lines (envd watch
// format), waking the sandbox if paused, until ctx ends, the directory goes
// away or maxFor passes. Each event counts as activity.
func (d *Daemon) WatchDir(ctx context.Context, id, dir string, maxFor time.Duration, onEvent func(line []byte)) error {
	if err := d.ensureRunning(ctx, id); err != nil {
		return err
	}
	inst, err := d.execInstance(id)
	if err != nil {
		return err
	}
	p, ok := inst.(rt.EnvdProtocoler)
	if !ok {
		return rt.ErrUnsupported
	}
	if lvl := p.EnvdProtocol(); lvl < envdproto.WatchCommand {
		return fmt.Errorf("%w: this sandbox's envd is protocol level %d and file watch needs %d; "+
			"rebuild the template", rt.ErrUnsupported, lvl, envdproto.WatchCommand)
	}

	// The watch's own deadline fires just before the exec's, so running out of
	// time ends the stream cleanly instead of as a failed exec.
	wctx, cancel := context.WithTimeout(ctx, maxFor)
	defer cancel()

	var pending []byte
	ready := false
	var stderr bytes.Buffer
	exit := 0
	err = d.ExecStream(wctx, id, rt.ExecSpec{
		Cmd:     watchCmd,
		Env:     map[string]string{"PD_WATCH_DIR": dir},
		Timeout: maxFor + 5*time.Second,
	}, func(f rt.StreamFrame) {
		stderr.Write(f.Stderr)
		if f.Exited {
			exit = f.ExitCode
		}
		pending = append(pending, f.Stdout...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			if i > 0 {
				ready = true
				d.Touch(id)
				onEvent(append([]byte(nil), pending[:i]...))
			}
			pending = pending[i+1:]
		}
	})
	if err != nil {
		if ready && errors.Is(wctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil
		}
		return err
	}
	// Failing before the first line is the watch never starting: a missing
	// directory, a file, or one the sandbox user cannot read. After it, an exit
	// is only the watch ending (its directory went away, or maxFor passed).
	if !ready && exit != 0 {
		return fmt.Errorf("%w: %s", rt.ErrPathNotFound, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}
