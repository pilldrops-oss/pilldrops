package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// reapForever is `envd reap`, the guest's PID 1 once init has execed it. It
// starts the envd server and starts it again whenever it exits (a tenant's
// `pkill envd` otherwise left exec answering 503 for good), and it waits on
// every other child, so an orphan that exits (a killed background job) is
// reaped instead of staying a zombie that still lists as running. It is a
// process of its own because a wait(-1) loop inside the server would steal the
// exit statuses its os/exec Waits depend on. server nil only reaps.
func reapForever(server []string) {
	// PID 1 dying panics the guest kernel, so `kill 1` or `pkill envd` must not
	// end it; the kernel only delivers to PID 1 the signals it handles. Caught
	// and dropped rather than ignored: an ignored signal stays ignored across
	// exec, and the server and everything it runs would inherit that. Never
	// read: once the buffer is full the runtime drops the rest.
	signal.Notify(make(chan os.Signal, 1))

	var (
		mu      sync.Mutex
		pid     int
		started time.Time
		policy  respawnPolicy
	)
	var start func()
	restartLater := func(ran time.Duration) {
		mu.Lock()
		delay, ok := policy.next(ran)
		mu.Unlock()
		if !ok {
			fmt.Fprintf(os.Stderr, "envd reap: the server exited %d times in a row; not starting it again\n", respawnLimit)
			return
		}
		time.AfterFunc(delay, start)
	}
	start = func() {
		mu.Lock()
		p, err := os.StartProcess(server[0], server, &os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
		if err == nil {
			pid, started = p.Pid, time.Now()
		}
		mu.Unlock()
		if err != nil {
			fmt.Fprintf(os.Stderr, "envd reap: start the server: %v\n", err)
			restartLater(0)
		}
	}
	if len(server) > 0 {
		start()
	}

	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, syscall.ECHILD) {
			time.Sleep(time.Second)
			continue
		}
		if err != nil || got <= 0 {
			continue
		}
		mu.Lock()
		wasServer := got == pid
		ran := time.Since(started)
		if wasServer {
			pid = 0
		}
		mu.Unlock()
		if wasServer {
			fmt.Fprintf(os.Stderr, "envd reap: the server exited (%v) after %v; starting it again\n", ws, ran.Round(time.Second))
			restartLater(ran)
		}
	}
}

const (
	respawnMin   = time.Second
	respawnMax   = 30 * time.Second
	respawnLimit = 10
	// healthyRun is how long a server must have run for its exit not to count
	// toward respawnLimit.
	healthyRun = time.Minute
)

// respawnPolicy spaces out restarts of the envd server: doubling from
// respawnMin to respawnMax, reset by a run of healthyRun, and given up after
// respawnLimit quick exits in a row, so a server that cannot start does not
// keep PID 1 busy forever.
type respawnPolicy struct{ fails int }

// next says how long to wait before starting the server again after a run
// that lasted ran, or ok=false to stop starting it.
func (p *respawnPolicy) next(ran time.Duration) (delay time.Duration, ok bool) {
	if ran >= healthyRun {
		p.fails = 0
	}
	p.fails++
	if p.fails > respawnLimit {
		return 0, false
	}
	return min(respawnMin<<(p.fails-1), respawnMax), true
}
