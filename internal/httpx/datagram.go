package httpx

import (
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
)

// UDP cannot be a byte splice: datagram boundaries and the sender's identity
// both have to survive the hop. One stream carries every client of one public
// port, framed as [len u16][addr 18 B][payload], and the addr is what lets the
// far end send each reply back to the client that asked.

// DatagramAddrLen is the wire size of an address: 16 bytes of IPv6-mapped
// address plus a big-endian port.
const DatagramAddrLen = 18

// MaxDatagram is the largest payload one frame carries, which is also the
// largest UDP payload IPv4 allows.
const MaxDatagram = 65507

// ErrDatagramTooLarge means the payload does not fit in one frame. Dropped
// rather than split: splitting would deliver two datagrams where one was sent.
var ErrDatagramTooLarge = errors.New("datagram is larger than one frame can carry")

// WriteDatagram writes one framed datagram. The caller serializes writes; a
// half-written frame desynchronizes the stream for every client on it.
func WriteDatagram(w io.Writer, from netip.AddrPort, payload []byte) error {
	if len(payload) > MaxDatagram {
		return ErrDatagramTooLarge
	}
	buf := make([]byte, 2+DatagramAddrLen+len(payload))
	// #nosec G115 -- the MaxDatagram guard above bounds this at 65507, which fits a uint16.
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload))) //nolint:gosec // G115, see the #nosec above
	a16 := from.Addr().As16()
	copy(buf[2:2+16], a16[:])
	binary.BigEndian.PutUint16(buf[2+16:2+DatagramAddrLen], from.Port())
	copy(buf[2+DatagramAddrLen:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadDatagram reads one framed datagram. The returned slice is freshly
// allocated, so it may be handed to another goroutine.
func ReadDatagram(r io.Reader) (netip.AddrPort, []byte, error) {
	var head [2 + DatagramAddrLen]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return netip.AddrPort{}, nil, err
	}
	n := binary.BigEndian.Uint16(head[:2])
	var a16 [16]byte
	copy(a16[:], head[2:2+16])
	addr := netip.AddrPortFrom(netip.AddrFrom16(a16).Unmap(), binary.BigEndian.Uint16(head[2+16:]))
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return netip.AddrPort{}, nil, err
	}
	return addr, payload, nil
}
