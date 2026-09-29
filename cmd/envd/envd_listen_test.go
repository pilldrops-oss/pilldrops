package main

import (
	"errors"
	"strings"
	"testing"
)

func TestEnvdListenAddr_RefusesRoutableWithoutHashOrChannel(t *testing.T) {
	// Nothing can authenticate a caller, so the routable bind must be refused.
	_, _, _, err := envdListenAddr(6001, "", false)
	if err == nil {
		t.Fatal("envd must refuse a routable bind with neither a token hash nor a credential channel")
	}
}

func TestEnvdListenAddr_AllowsRoutableWithHash(t *testing.T) {
	_, addr, reason, err := envdListenAddr(6001, "deadbeefhash", false)
	if err != nil {
		t.Fatalf("with a hash, bind must be allowed: %v", err)
	}
	if addr == "" {
		t.Fatal("expected a bind address")
	}
	if reason == "" {
		t.Fatal("expected a stated reason for the bind; an unexplained transport choice is the defect this phase exists to fix")
	}
}

// A VM booted with no credential must still come up when a channel can deliver one later.
func TestEnvdListenAddr_AllowsRoutableWithChannelAndNoHash(t *testing.T) {
	network, addr, reason, err := envdListenAddr(6001, "", true)
	if err != nil {
		t.Fatalf("a live credential channel must permit the bind: %v", err)
	}
	if network != "tcp" || addr == "" {
		t.Fatalf("network=%q addr=%q; the data path stays TCP in this phase", network, addr)
	}
	if !strings.Contains(reason, "refused") {
		t.Fatalf("reason %q must say that authenticated methods are refused until a credential arrives; "+
			"a live channel is not permission to call anything", reason)
	}
}

// The host dials envd over TCP (internal/vm/coldboot.go), so the data path
// must never bind vsock unless the host dialer changes in the same commit.
func TestEnvdListenAddr_NeverReturnsVsock(t *testing.T) {
	for _, tc := range []struct {
		hash    string
		channel bool
	}{
		{"deadbeefhash", false},
		{"deadbeefhash", true},
		{"", true},
	} {
		network, _, _, err := envdListenAddr(6001, tc.hash, tc.channel)
		if err != nil {
			t.Fatalf("hash=%q channel=%v: %v", tc.hash, tc.channel, err)
		}
		if network != "tcp" {
			t.Fatalf("hash=%q channel=%v: network=%q — the host dials envd over TCP "+
				"(internal/vm/coldboot.go). Binding vsock here breaks every Firecracker create; "+
				"switch the host dialer in the same change or leave this alone.",
				tc.hash, tc.channel, network)
		}
	}
}

// The notice must carry the actual vsock error, not a generic "unavailable".
func TestTransportNoticeNamesTheVsockFailure(t *testing.T) {
	got := transportNotice("tcp", "boot provisioned a token hash", 6002,
		errors.New("vsock socket: address family not supported by protocol"))

	for _, want := range []string{
		"vsock",
		"6002",
		"address family not support",
		"ENVD_TOKEN_HASH",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transport notice does not contain %q; a fallback nobody can read is the defect this fixes.\ngot:\n%s", want, got)
		}
	}
}

func TestTransportNoticeReportsALiveChannel(t *testing.T) {
	got := transportNotice("tcp", "boot provisioned a token hash", 6002, nil)
	if !strings.Contains(got, "6002") || !strings.Contains(got, "listening") {
		t.Fatalf("a working credential channel must be stated too, not only a broken one:\n%s", got)
	}
	if strings.Contains(got, "UNAVAILABLE") {
		t.Fatalf("reported a live channel as unavailable:\n%s", got)
	}
}

func TestTransportNoticeLogsOnce(t *testing.T) {
	var b strings.Builder
	var o onceLogger
	o.log(&b, "notice\n")
	o.log(&b, "notice\n")
	o.log(&b, "notice\n")
	if n := strings.Count(b.String(), "notice"); n != 1 {
		t.Fatalf("transport notice printed %d times, want exactly 1", n)
	}
}
