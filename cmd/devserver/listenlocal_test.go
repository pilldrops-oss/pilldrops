package main

import (
	"net"
	"strings"
	"testing"
)

// Another program holding [::1]:port would answer http://localhost:port in a
// browser; the dev server must refuse to start rather than share the name.
func TestListenLocalRefusesAPortTakenOnIPv6Loopback(t *testing.T) {
	other, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	defer other.Close()
	_, port, _ := net.SplitHostPort(other.Addr().String())

	if _, err := listenLocal(net.JoinHostPort("127.0.0.1", port)); err == nil || !strings.Contains(err.Error(), "[::1]:"+port) {
		t.Fatalf("listenLocal = %v, want a refusal naming [::1]:%s", err, port)
	}
}

func TestListenLocalServesBothLoopbacks(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(probe.Addr().String())
	probe.Close()

	ls, err := listenLocal(net.JoinHostPort("127.0.0.1", port))
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
