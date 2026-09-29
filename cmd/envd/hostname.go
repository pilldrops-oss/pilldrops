package main

import (
	"fmt"
	"os"
	"regexp"
)

// credSetHost renames the guest: `SETHOST <name>`.
//
// Every machine stamped out of one memory image comes up believing it is the
// machine that was captured — same hostname in the shell prompt, in `uname -n`,
// and in whatever the app logs. This is the first path where two live machines
// share a memory image and a user can see it, so the node pushes each child its
// own name after it has pushed its address.
const credSetHost = "SETHOST"

// hostnameRe is what the kernel and /etc/hosts will accept without complaint.
// Sandbox ids satisfy it; the bound is Linux's HOST_NAME_MAX.
var hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,63}$`)

// applyHostname sets the running kernel's hostname and rewrites /etc/hostname
// so it survives anything in the guest that re-reads it. The live value is what
// a shell prompt shows, and the file is what a reboot or a service restart
// would take, so both or neither.
func applyHostname(name string) error {
	if !hostnameRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid hostname", name)
	}
	if err := setHostname(name); err != nil {
		return err
	}
	// Best effort: a read-only /etc is not a reason to report the live rename
	// as failed, and the node is about to hand this sandbox out.
	_ = os.WriteFile("/etc/hostname", []byte(name+"\n"), 0o644) //nolint:errcheck // see above
	return nil
}
