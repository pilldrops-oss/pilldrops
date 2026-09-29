package httpapi

// The node end of a UDP forward. One hijacked stream carries every client of
// one public port; the node keeps a guest socket per client address so replies
// find their way back to the client that asked, and expires it when idle.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// udpIdleTimeout retires a client's guest socket after this long with no
// traffic in either direction. Without it one stream would leak a socket per
// client address for as long as it stays open.
const udpIdleTimeout = 60 * time.Second

// udpSweepEvery is how often idle peers are collected.
const udpSweepEvery = 15 * time.Second

func (s *Server) handleForwardUDP(w http.ResponseWriter, r *http.Request) {
	target, port, ok := s.forwardTarget(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := net.ResolveUDPAddr("udp", target); err != nil {
		writeErr(w, http.StatusBadGateway, "guest port "+r.PathValue("port")+" is not addressable over udp")
		return
	}
	stream, err := hijackRaw(w)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := io.WriteString(stream, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Connection: Upgrade\r\nUpgrade: pilldrops-l4-udp\r\n\r\n"); err != nil {
		_ = stream.Close() //nolint:errcheck // best-effort
		return
	}
	slog.InfoContext(r.Context(), "udp forward open",
		"component", "forward", "sandbox", id, "port", port, "target", target)
	// Not r.Context(): the request is hijacked and the relay outlives it.
	relayUDP(context.Background(), stream, target)
}

// udpPeer is one client address's socket to the guest.
type udpPeer struct {
	conn *net.UDPConn
	// last is the unix-nano of the most recent frame either way, read under the
	// relay's mutex.
	last time.Time
}

// relayUDP pumps framed datagrams between stream and the guest at target until
// the stream ends. Returns when the stream is done; every peer socket is closed.
func relayUDP(ctx context.Context, stream net.Conn, target string) {
	// Two locks on purpose: a reply goroutine must not hold the peer map while
	// writing to the stream. The write blocks whenever the far end is slow, and
	// with one lock the main loop would block on the map and stop draining the
	// stream, which is the far end's back-pressure feeding back on itself.
	// (Structural, not the cause of the 200-datagram hang seen live — that was
	// the guest's own UDP receive buffer dropping an unpaced burst.)
	var peersMu sync.Mutex
	var writeMu sync.Mutex // serializes frames onto the stream; never held with peersMu
	peers := map[netip.AddrPort]*udpPeer{}
	defer func() {
		peersMu.Lock()
		for _, p := range peers {
			_ = p.conn.Close() //nolint:errcheck // best-effort
		}
		peers = nil
		peersMu.Unlock()
		_ = stream.Close() //nolint:errcheck // best-effort
	}()

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(udpSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				peersMu.Lock()
				for addr, p := range peers {
					if now.Sub(p.last) > udpIdleTimeout {
						_ = p.conn.Close() //nolint:errcheck // best-effort; its reader exits
						delete(peers, addr)
					}
				}
				peersMu.Unlock()
			}
		}
	}()
	defer close(done)

	for {
		addr, payload, err := httpx.ReadDatagram(stream)
		if err != nil {
			return
		}
		peersMu.Lock()
		p := peers[addr]
		if p == nil {
			c, dErr := (&net.Dialer{Timeout: forwardDialTimeout}).DialContext(ctx, "udp", target)
			if dErr != nil {
				peersMu.Unlock()
				continue // drop, like any unreachable UDP destination
			}
			p = &udpPeer{conn: c.(*net.UDPConn)}
			peers[addr] = p
			// One reader per client socket, framing replies back with the client's
			// own address. Without the address a second client on the same public
			// port would receive the first one's replies.
			go func(addr netip.AddrPort, p *udpPeer) {
				buf := make([]byte, httpx.MaxDatagram)
				for {
					n, rErr := p.conn.Read(buf)
					if rErr != nil {
						return
					}
					peersMu.Lock()
					p.last = time.Now()
					peersMu.Unlock()
					writeMu.Lock()
					wErr := httpx.WriteDatagram(stream, addr, buf[:n])
					writeMu.Unlock()
					if wErr != nil {
						return
					}
				}
			}(addr, p)
		}
		p.last = time.Now()
		conn := p.conn
		peersMu.Unlock()
		_, _ = conn.Write(payload) //nolint:errcheck // a dropped datagram is UDP working as designed
	}
}
