//go:build linux

package egressproxy

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// originalDst returns the destination the guest actually dialled, before the
// namespace's `redirect` rewrote it. Conntrack holds it, and SO_ORIGINAL_DST
// is how a redirected socket asks — which is the whole reason the divert is a
// `redirect` in the namespace's own prerouting chain and not a DNAT to a
// node-wide listener: a socket on the far side of a different namespace's NAT
// has no conntrack entry to ask, and the destination is simply gone.
func originalDst(c net.Conn) (netip.AddrPort, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, errors.New("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var (
		mreq   *unix.IPv6Mreq
		optErr error
	)
	if err := raw.Control(func(fd uintptr) {
		// IPv6Mreq is 16 bytes of storage borrowed to hold a sockaddr_in;
		// x/sys has no typed getter for this option and the kernel writes the
		// same struct either way. Layout: family[0:2] port[2:4] addr[4:8].
		mreq, optErr = unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
	}); err != nil {
		return netip.AddrPort{}, err
	}
	if optErr != nil {
		return netip.AddrPort{}, fmt.Errorf("SO_ORIGINAL_DST: %w", optErr)
	}
	b := mreq.Multiaddr
	port := uint16(b[2])<<8 | uint16(b[3])
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]}), port), nil
}
