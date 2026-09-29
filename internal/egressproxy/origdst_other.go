//go:build !linux

package egressproxy

import (
	"errors"
	"net"
	"net/netip"
)

// originalDst needs conntrack and SO_ORIGINAL_DST, which is a Linux redirect.
// Everything else in this package is portable so the sniffing and the verdict
// are asserted on a macOS checkout, where the nodes' build tag compiles for
// nobody — the lesson netnsruleset.go already records.
func originalDst(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("the name layer needs a Linux netfilter redirect")
}
