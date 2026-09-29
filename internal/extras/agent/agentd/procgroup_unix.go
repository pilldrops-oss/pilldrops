//go:build unix

package agentd

import (
	"os/exec"
	"syscall"
)

// Turns are killed as a process group so the shell's children (npm, git, test
// runners) die with it.

func setProcessGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to the whole group led by pid.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

var (
	sigInterrupt = syscall.SIGINT
	sigKill      = syscall.SIGKILL
)
