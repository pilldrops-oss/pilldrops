package main

import (
	"net"
	"strings"
	"testing"
)

// Another program holding [::1]:port would answer http://localhost:port in a
// browser; the dev server must refuse to start rather than share the name.
func TestListenLocalRefusesAPortTakenOnIPv6Loopback(t *testing.T) {
	// The port is free on [::1] only by construction; on 127.0.0.1 some other
	// process may hold it, which is a different refusal. Try another port then.
	for range 20 {
		other, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Skipf("no IPv6 loopback here: %v", err)
		}
		_, port, _ := net.SplitHostPort(other.Addr().String())
		lns, err := listenLocal(t.Context(), net.JoinHostPort("127.0.0.1", port))
		_ = other.Close()
		for _, l := range lns {
			_ = l.Close()
		}
		if err != nil && strings.Contains(err.Error(), "listen tcp 127.0.0.1:"+port) {
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "[::1]:"+port) {
			t.Fatalf("listenLocal = %v, want a refusal naming [::1]:%s", err, port)
		}
		return
	}
	t.Skip("every [::1] port tried was taken on 127.0.0.1")
}

func TestListenLocalServesBothLoopbacks(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(probe.Addr().String())
	probe.Close()

	ls, err := listenLocal(t.Context(), net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		defer l.Close()
	}
	if _, err := net.Listen("tcp", "[::1]:0"); err == nil && len(ls) != 2 {
		t.Fatalf("got %d listeners, want IPv4 and IPv6 loopback", len(ls))
	}
}
