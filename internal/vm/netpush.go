//go:build linux

package vm

// Host end of the network-rewrite channel. Every guest restored from one golden
// snapshot comes up with the golden's address and MAC; the node pushes the
// sandbox's own identity over the per-VM vsock UDS, which works while eth0 is wrong.

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ErrGuestNetworkPush wraps every network-rewrite failure.
var ErrGuestNetworkPush = errors.New("could not give the resumed guest its own network identity")

// guestNetwork is the identity a restored guest is told to take.
type guestNetwork struct {
	IP      net.IP
	Mask    net.IPMask
	Gateway net.IP
	MAC     string
	Device  string
}

// guestNetworkFor builds the identity for the address the node allocated. The MAC
// must be rewritten too: fcbr0 learns a MAC on whichever port last transmitted,
// so two guests sharing the golden MAC get each other's replies.
func guestNetworkFor(ip net.IP) guestNetwork {
	return guestNetwork{
		IP:      ip,
		Mask:    net.CIDRMask(16, 32), // fcbr0 is a /16; matches buildKernelArgs
		Gateway: GatewayIP,
		// The kernel renders /sys/class/net/eth0/address in lower case; match it so the two compare equal.
		MAC:    strings.ToLower(generateMAC(ip)),
		Device: "eth0", // matches buildKernelArgs
	}
}

// command renders the SETNET line; the format is fixed by cmd/envd/netconfig.go.
func (g guestNetwork) command() string {
	ones, _ := g.Mask.Size()
	return fmt.Sprintf("%s %s/%d %s %s %s",
		credSetNetCommand, g.IP.String(), ones, g.Gateway.String(), g.MAC, g.Device)
}

// deliverGuestNetworkFn is a seam so the live suite can suppress the rewrite.
var deliverGuestNetworkFn = deliverGuestNetwork

// deliverGuestNetwork tells the resumed guest its address, MAC and route,
// retrying until deadline because envd's listener is not necessarily back yet.
func deliverGuestNetwork(chrootRoot, sandboxID string, g guestNetwork, deadline time.Duration) error {
	if g.IP == nil || g.Gateway == nil || g.MAC == "" || g.Device == "" {
		return fmt.Errorf("%w: incomplete network for %s (%+v)", ErrGuestNetworkPush, sandboxID, g)
	}
	cmd := g.command()
	if err := pushToGuest(chrootRoot, sandboxID, cmd, deadline); err != nil {
		return fmt.Errorf("%w: %w", ErrGuestNetworkPush, err)
	}
	return nil
}

// credSetHostCommand mirrors cmd/envd/hostname.go's credSetHost.
const credSetHostCommand = "SETHOST"

// hostnamePushDeadline matches the network push: this runs in the same window,
// against a guest whose envd has just answered SETNET.
const hostnamePushDeadline = 3 * time.Second

// deliverGuestHostname gives a restored guest its own name.
//
// Every machine restored from one memory image comes up believing it is the
// machine that was captured: same hostname in the prompt, in `uname -n`, and in
// whatever the app writes to its logs. Two sandboxes stamped out of one named
// snapshot are the first case where that is visible to a user rather than
// merely untidy, so the name is pushed alongside the address.
//
// Not fatal on failure: a child with the wrong name is wrong in a way the user
// can see and work around, and tearing down a machine that is otherwise healthy
// over a cosmetic field is the worse trade. The caller logs it.
func deliverGuestHostname(chrootRoot, sandboxID string) error {
	cmd := fmt.Sprintf("%s %s", credSetHostCommand, sandboxID)
	return pushToGuest(chrootRoot, sandboxID, cmd, hostnamePushDeadline)
}

// credSetTimeCommand mirrors cmd/envd/credential.go's credSetTime.
const credSetTimeCommand = "SETTIME"

// clockPushDeadline is short: the guest has already answered an authenticated
// envd call by the time this runs, so its listener is up or something else is wrong.
const clockPushDeadline = 3 * time.Second

// deliverGuestClock steps a restored guest's wall clock to now.
//
// A guest restored from a memory image comes back believing the time is
// whatever it was when that image was written: measured on the reference host,
// 62 seconds of host time across a pause/resume advanced the guest's clock by
// 0. There is no NTP, no virtio-rtc and no hypervisor time device in these
// VMs, so nothing in the guest ever notices. Everything that checks a
// certificate, signs a request or writes a timestamp is wrong by the length of
// the pause, which for an overnight one is hours.
//
// Freeze/thaw (standby) needs no push: measured across a 60 s freeze the guest
// tracked the host to within 1 s, because the vCPUs stop but the clocksource
// does not.
func deliverGuestClock(chrootRoot, sandboxID string) error {
	cmd := fmt.Sprintf("%s %d", credSetTimeCommand, time.Now().UnixNano())
	return pushToGuest(chrootRoot, sandboxID, cmd, clockPushDeadline)
}

// pushToGuest sends one command over the VM's vsock UDS, retrying until deadline.
// Returns the last error; the first is usually a "connection refused" from before the socket existed.
func pushToGuest(chrootRoot, sandboxID, command string, deadline time.Duration) error {
	return pushToGuestWaitingOutStalls(chrootRoot, sandboxID, command, deadline, 0)
}

// pushToGuestWaitingOutStalls is pushToGuest with a second, larger deadline that
// applies only once the guest has proved it is alive by accepting a connection
// nothing inside it then answered (ErrGuestNoReply). A guest that never accepts
// keeps the short deadline, so a genuinely absent envd is still declared absent
// in `deadline`. Pass 0 for stalled to opt out, which every best-effort push
// does: a cosmetic SETHOST must not hold a resume open for a quarter of an hour.
func pushToGuestWaitingOutStalls(chrootRoot, sandboxID, command string, deadline, stalled time.Duration) error {
	until := time.Now().Add(deadline)
	effective := deadline
	var last error
	for {
		udsPath, err := vsockUDSPath(chrootRoot, sandboxID)
		if err == nil {
			if err = pushCommand(udsPath, credVsockPort, command); err == nil {
				return nil
			}
		}
		last = err
		if errors.Is(err, ErrGuestNoReply) && stalled > effective {
			// The guest is there and stalled, not gone. Wait it out from the
			// start of this push, not from now, so the bound stays a bound.
			until = until.Add(stalled - effective)
			effective = stalled
		}
		if time.Now().After(until) {
			return fmt.Errorf("not accepted by the resumed guest within %v: %w", effective, last)
		}
		time.Sleep(credPushRetryInterval)
	}
}
