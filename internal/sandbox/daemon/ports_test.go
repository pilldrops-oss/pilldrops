package daemon

import (
	"context"
	"net"
	"testing"
)

func TestDetectPortsFindsTheOneListening(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()

	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A real listener on a free port; the fake's ListenTarget resolves to it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	listeningPort := ln.Addr().(*net.TCPAddr).Port

	// Take a free port from the OS rather than listeningPort+1, which parallel tests may hold.
	silentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	silentPort := silentLn.Addr().(*net.TCPAddr).Port
	silentLn.Close()

	got, err := d.DetectPorts(ctx, sb.ID, []int{listeningPort, silentPort})
	if err != nil {
		t.Fatalf("DetectPorts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	want := map[int]bool{listeningPort: true, silentPort: false}
	for _, ps := range got {
		if ps.Listening != want[ps.Port] {
			t.Fatalf("port %d listening=%v, want %v", ps.Port, ps.Listening, want[ps.Port])
		}
	}
}

func TestDetectPortsUnknownSandbox(t *testing.T) {
	d, _ := newTestDaemon(t)
	if _, err := d.DetectPorts(context.Background(), "nope", []int{8000}); err == nil {
		t.Fatal("want error for unknown sandbox")
	}
}

// Lines shaped like a real guest's /proc/net/tcp and tcp6.
const procNetSample = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 1 1
   1: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 2 1
   2: 0100007F:1F90 0100007F:D2A4 01 00000000:00000000 00:00000000 00000000  1001        0 3 1
   3: 0100007F:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 4 1
   4: 0B00007F:8E53 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9 1
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1538 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 5 1
   1: 00000000000000000000000000000000:1EAB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 6 1
   2: 0000000000000000FFFF00000100007F:22B8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 7 1
   3: 00000000000000000000000001000000:1435 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 8 1
`

func TestParseProcNetTCP(t *testing.T) {
	got := parseProcNetTCP(procNetSample)
	want := map[int]bool{
		8000: false, // 0.0.0.0: reachable
		5173: true,  // vite's default 127.0.0.1, and ::1 too
		5432: true,  // 127.0.0.1 and ::1: still loopback only
		7851: false, // [::]
		8888: true,  // ::ffff:127.0.0.1
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (8080 is ESTABLISHED, not LISTEN, and 127.0.0.11 is Docker DNS: both ignored)", got, want)
	}
	for port, loop := range want {
		if g, ok := got[port]; !ok || g != loop {
			t.Errorf("port %d: got (loopbackOnly=%v, present=%v), want loopbackOnly=%v", port, g, ok, loop)
		}
	}
}
