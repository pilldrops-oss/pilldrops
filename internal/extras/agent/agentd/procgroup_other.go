//go:build !unix

package agentd

import (
	"os"
	"os/exec"
	"syscall"
)

// Non-unix build stub: interrupt degrades to signalling the single child.

func setProcessGroup(*exec.Cmd) {}

func signalProcessGroup(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

var (
	sigInterrupt = syscall.Signal(syscall.SIGINT)
	sigKill      = syscall.Signal(syscall.SIGKILL)
)
