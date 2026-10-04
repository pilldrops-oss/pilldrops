package main

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

// The credential protocol is transport-independent, so it is tested here on a
// loopback listener; the AF_VSOCK plumbing is covered by vsock_linux_test.go.

func TestHandleCredCommand(t *testing.T) {
	_, good := sbtoken.Mint()

	for _, tc := range []struct {
		name, line, wantReply, wantStored string
	}{
		{"sets a valid hash", credSetHash + " " + good + "\n", "OK", good},
		{"rejects a short hash", credSetHash + " deadbeef\n", "ERR", ""},
		{"rejects upper case", credSetHash + " " + strings.ToUpper(good) + "\n", "ERR", ""},
		{"rejects non-hex", credSetHash + " " + strings.Repeat("z", 64) + "\n", "ERR", ""},
		{"rejects a missing argument", credSetHash + "\n", "ERR", ""},
		{"rejects two arguments", credSetHash + " " + good + " " + good + "\n", "ERR", ""},
		{"rejects an unknown command", "GETHASH\n", "ERR", ""},
		{"rejects an empty line", "\n", "ERR", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hashes := newHashStore("")
			reply := handleCredCommand(tc.line, hashes)
			if !strings.HasPrefix(reply, tc.wantReply) {
				t.Fatalf("reply = %q, want prefix %q", reply, tc.wantReply)
			}
			if got := hashes.Get(); got != tc.wantStored {
				t.Fatalf("stored %q, want %q — a refused command must not change the credential", got, tc.wantStored)
			}
		})
	}
}

// Firecracker acks a CONNECT at the kernel level, so the node must read a
// reply this process wrote after doing the work.
func TestCredChannelRepliesWithAPayloadNotAnAck(t *testing.T) {
	_, hash := sbtoken.Mint()
	hashes := newHashStore("")
	l := credChannelOnLoopback(t, hashes)

	reply := pushCredential(t, l.Addr().String(), credSetHash+" "+hash)
	if reply != "OK" {
		t.Fatalf("reply = %q, want OK", reply)
	}
	if hashes.Get() != hash {
		t.Fatal("the channel replied OK without storing the credential")
	}
}

// The same server and token, refused before the push and allowed after;
// asserting only "allowed after" would pass with no authentication at all.
func TestUncredentialedServerRefusesUntilTheChannelDelivers(t *testing.T) {
	old := homeDir
	homeDir = t.TempDir()
	t.Cleanup(func() { homeDir = old })

	token, hash := sbtoken.Mint()
	hashes := newHashStore("") // a golden/restored guest: no boot credential
	client := envdOnLoopback(t, hashes)
	authed := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(sbtoken.MetadataKey, token))

	if _, err := client.Health(context.Background(), &pb.HealthRequest{}); err != nil {
		t.Fatalf("Health must stay open: %v", err)
	}
	if _, err := client.ListFiles(authed, &pb.ListFilesRequest{Path: "."}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("before the push, the correct token was ACCEPTED (err=%v). "+
			"A live credential channel is not permission to call anything.", err)
	}

	l := credChannelOnLoopback(t, hashes)
	if reply := pushCredential(t, l.Addr().String(), credSetHash+" "+hash); reply != "OK" {
		t.Fatalf("push reply = %q", reply)
	}

	// Same connection, so this proves the interceptors read the store at call time.
	if _, err := client.ListFiles(authed, &pb.ListFilesRequest{Path: "."}); err != nil {
		t.Fatalf("after the push, the correct token was refused: %v", err)
	}
}

// A hash delivered over one sandbox's channel authorizes that sandbox and nothing else.
func TestCrossSandboxTokenIsRefused(t *testing.T) {
	old := homeDir
	homeDir = t.TempDir()
	t.Cleanup(func() { homeDir = old })

	tokenA, hashA := sbtoken.Mint()
	tokenB, hashB := sbtoken.Mint()

	storeA, storeB := newHashStore(""), newHashStore("")
	clientA, clientB := envdOnLoopback(t, storeA), envdOnLoopback(t, storeB)

	lA := credChannelOnLoopback(t, storeA)
	lB := credChannelOnLoopback(t, storeB)
	if r := pushCredential(t, lA.Addr().String(), credSetHash+" "+hashA); r != "OK" {
		t.Fatalf("push to A: %q", r)
	}
	if r := pushCredential(t, lB.Addr().String(), credSetHash+" "+hashB); r != "OK" {
		t.Fatalf("push to B: %q", r)
	}

	ctxA := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(sbtoken.MetadataKey, tokenA))
	ctxB := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(sbtoken.MetadataKey, tokenB))

	// Positive controls first, or "refused" below could mean the credential never took effect.
	if _, err := clientA.ListFiles(ctxA, &pb.ListFilesRequest{Path: "."}); err != nil {
		t.Fatalf("A's own token refused by A: %v", err)
	}
	if _, err := clientB.ListFiles(ctxB, &pb.ListFilesRequest{Path: "."}); err != nil {
		t.Fatalf("B's own token refused by B: %v", err)
	}

	if _, err := clientB.ListFiles(ctxA, &pb.ListFilesRequest{Path: "."}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("sandbox A's token drove sandbox B (err=%v)", err)
	}
	if _, err := clientA.ListFiles(ctxB, &pb.ListFilesRequest{Path: "."}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("sandbox B's token drove sandbox A (err=%v)", err)
	}
}

func TestCredChannelAllowedRefusesOutsideAGuest(t *testing.T) {
	const guestCID = 3

	if ok, why := credChannelAllowed("", guestCID); !ok {
		t.Fatalf("a Firecracker guest with no boot credential must get a channel; refused: %s", why)
	}

	for _, tc := range []struct {
		name    string
		envHash string
		cid     uint32
		wantWhy string
	}{
		{"docker container", "0f" + strings.Repeat("0", 62), guestCID, "ENVD_TOKEN_HASH"},
		{"host vsock namespace", "", 2 /* VMADDR_CID_HOST */, "context id"},
		{"loopback vsock only", "", 1 /* VMADDR_CID_LOCAL */, "context id"},
		{"hypervisor", "", 0, "context id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := credChannelAllowed(tc.envHash, tc.cid)
			if ok {
				t.Fatalf("opened a credential channel outside a guest VM: a neighbour could push "+
					"a hash it knows and drive this sandbox as root (env=%q cid=%d)", tc.envHash, tc.cid)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Fatalf("refusal %q does not name %q", why, tc.wantWhy)
			}
		})
	}
}

// The listener binds VMADDR_CID_ANY, so a tenant process could reach it over loopback.
func TestCredChannelRefusesANonHostPeer(t *testing.T) {
	_, hash := sbtoken.Mint()
	hashes := newHashStore("")
	l := credChannelOnLoopbackAs(t, hashes, testGuestCID)

	reply := pushCredential(t, l.Addr().String(), credSetHash+" "+hash)
	if !strings.HasPrefix(reply, "ERR") {
		t.Fatalf("a guest-local peer pushed a credential and got %q", reply)
	}
	if hashes.Get() != "" {
		t.Fatal("a guest-local peer set this sandbox's credential; it could now lock the node out")
	}

	// Positive control: from the host it works, so the refusal above is not a broken channel.
	hashes2 := newHashStore("")
	lh := credChannelOnLoopbackAs(t, hashes2, testHostCID)
	if r := pushCredential(t, lh.Addr().String(), credSetHash+" "+hash); r != "OK" {
		t.Fatalf("the host was refused too (%q) — the test proves nothing", r)
	}
}

func TestCredPeerAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr net.Addr
		want bool
	}{
		{"host", testVsockAddr{cid: 2}, true},
		{"this guest", testVsockAddr{cid: 3}, false},
		{"another guest", testVsockAddr{cid: 42}, false},
		{"vsock loopback", testVsockAddr{cid: 1}, false},
		{"hypervisor", testVsockAddr{cid: 0}, false},
		{"not vsock at all", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6002}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := credPeerAllowed(tc.addr)
			if got != tc.want {
				t.Fatalf("credPeerAllowed(%v) = %v, want %v (%s)", tc.addr, got, tc.want, why)
			}
		})
	}
}

func TestCredChannelBoundsALine(t *testing.T) {
	hashes := newHashStore("")
	l := credChannelOnLoopback(t, hashes)

	_, hash := sbtoken.Mint()
	long := strings.Repeat("A", credMaxLine*4) + " " + credSetHash + " " + hash
	_ = pushCredential(t, l.Addr().String(), long)
	if hashes.Get() != "" {
		t.Fatal("an over-long line installed a credential")
	}
}

const testHostCID, testGuestCID = 2, 3

// credChannelOnLoopback runs the real accept loop over a TCP listener whose
// connections report a vsock peer address, so credPeerAllowed is exercised.
func credChannelOnLoopback(t *testing.T, hashes *hashStore) net.Listener {
	return credChannelOnLoopbackAs(t, hashes, testHostCID)
}

func credChannelOnLoopbackAs(t *testing.T, hashes *hashStore, cid uint32) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go serveCredChannel(&peerListener{Listener: l, cid: cid}, hashes)
	return l
}

// peerListener re-labels accepted connections with a vsock peer address.
type peerListener struct {
	net.Listener
	cid uint32
}

func (p *peerListener) Accept() (net.Conn, error) {
	c, err := p.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &peerConn{Conn: c, cid: p.cid}, nil
}

type peerConn struct {
	net.Conn
	cid uint32
}

func (p *peerConn) RemoteAddr() net.Addr { return testVsockAddr{cid: p.cid} }

type testVsockAddr struct{ cid uint32 }

func (a testVsockAddr) Network() string   { return "vsock" }
func (a testVsockAddr) String() string    { return "vsock(test)" }
func (a testVsockAddr) ContextID() uint32 { return a.cid }

// pushCredential writes one command and returns the reply line, trimmed.
func pushCredential(t *testing.T, addr, command string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial credential channel: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(command + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil && line == "" {
		t.Fatalf("read reply: %v", err)
	}
	return strings.TrimSpace(line)
}

// envdOnLoopback starts a real gRPC envd backed by hashes and returns a client.
func envdOnLoopback(t *testing.T, hashes *hashStore) pb.EnvdServiceClient {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(tokenAuthOptions(hashes)...)
	pb.RegisterEnvdServiceServer(srv, &envdServer{})
	go srv.Serve(l)
	t.Cleanup(func() { srv.Stop(); l.Close() })

	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial envd: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewEnvdServiceClient(conn)
}

// A restored guest gets its credential once; a server the reaper starts again
// must come back with it, not refuse every call.
func TestARestartedServerKeepsThePushedHash(t *testing.T) {
	t.Setenv("ENVD_TOKEN_HASH", "")
	path := filepath.Join(t.TempDir(), "hash")
	pushed := strings.Repeat("ab", 32)
	if reply := handleCredCommand(credSetHash+" "+pushed, persistedHashStore(path)); reply != "OK" {
		t.Fatalf("push = %q", reply)
	}
	if got := persistedHashStore(path).Get(); got != pushed {
		t.Fatalf("restarted server hash = %q, want the pushed %q", got, pushed)
	}
	t.Setenv("ENVD_TOKEN_HASH", strings.Repeat("cd", 32))
	if got := persistedHashStore(path).Get(); got != strings.Repeat("cd", 32) {
		t.Fatalf("docker's env hash lost to a file: %q", got)
	}
}
