//go:build linux

package envd

import (
	"net"
	"testing"
)

// Puts a listener on localhost:6001 so the test can tell "refused" from "nothing there".
func TestDialVsockRefusesEmptyRatherThanUsingLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:6001")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1:6001 (%v); without a listener this test cannot tell refusal from a dead port", err)
	}
	defer ln.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()

	conn, err := dialVsock("")
	if err == nil {
		conn.Close()
		t.Fatal("dialVsock(\"\") connected; it must refuse an empty address, not fall back to the node's loopback")
	}

	select {
	case <-accepted:
		t.Fatal("dialVsock(\"\") reached the listener on localhost:6001 — it is still substituting a default address")
	default:
	}
}
