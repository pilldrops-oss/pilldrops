package main

import (
	"errors"
	"syscall"
	"time"
)

// reapForever is `envd reap`, the guest's PID 1 once init has started the envd
// server: it only waits on children, so an orphan that exits (a killed
// background job) is reaped instead of staying a zombie that still lists as
// running. It is a process of its own because a wait(-1) loop inside the server
// would steal the exit statuses its os/exec Waits depend on.
func reapForever() {
	for {
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(-1, &ws, 0, nil); errors.Is(err, syscall.ECHILD) {
			time.Sleep(time.Second)
		}
	}
}
