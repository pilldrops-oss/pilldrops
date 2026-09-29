package main

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// theWireLine is the exact line a node sends, duplicated verbatim in internal/vm's
// TestSetNetWireFormat; the literal is the contract pinned from both sides.
const theWireLine = "SETNET 192.168.0.7/16 192.168.30.1 aa:fc:c0:a8:00:07 eth0"

func TestParseNetConfig_TheWireLine(t *testing.T) {
	fields := strings.Fields(theWireLine)
	if fields[0] != credSetNet {
		t.Fatalf("the wire line starts with %q, not %q", fields[0], credSetNet)
	}
	c, err := parseNetConfig(fields[1:])
	if err != nil {
		t.Fatalf("parseNetConfig(%q): %v", theWireLine, err)
	}

	if got := c.Addr.IP.String(); got != "192.168.0.7" {
		t.Errorf("address = %s, want 192.168.0.7 (the host address, not the network)", got)
	}
	if ones, bits := c.Addr.Mask.Size(); ones != 16 || bits != 32 {
		t.Errorf("mask = /%d of %d bits, want /16 of 32", ones, bits)
	}
	if got := c.Gateway.String(); got != "192.168.30.1" {
		t.Errorf("gateway = %s", got)
	}
	if got := c.MAC.String(); got != "aa:fc:c0:a8:00:07" {
		t.Errorf("mac = %s", got)
	}
	if c.Device != "eth0" {
		t.Errorf("device = %s", c.Device)
	}
}

func TestParseNetConfig_Rejections(t *testing.T) {
	base := []string{"192.168.0.7/16", "192.168.30.1", "aa:fc:c0:a8:00:07", "eth0"}
	repl := func(i int, v string) []string {
		out := append([]string(nil), base...)
		out[i] = v
		return out
	}
	cases := []struct {
		name     string
		fields   []string
		wantWord string
	}{
		{"too few", base[:3], "four arguments"},
		{"too many", append(append([]string(nil), base...), "extra"), "four arguments"},
		{"address without a prefix", repl(0, "192.168.0.7"), "argument 1"},
		{"ipv6", repl(0, "fd00::1/64"), "argument 1"},
		{"gateway with a prefix", repl(1, "192.168.30.1/16"), "argument 2"},
		{"gateway is not an address", repl(1, "gateway"), "argument 2"},
		{"mac is not a mac", repl(2, "aa:fc:c0:a8:00"), "argument 3"},
		{"mac is 8 bytes", repl(2, "aa:fc:c0:a8:00:07:11:22"), "argument 3"},
		{"device is empty", repl(3, ""), "argument 4"},
		{"device is a path", repl(3, "../../etc"), "argument 4"},
		{"device is too long", repl(3, strings.Repeat("e", maxIfaceName+1)), "argument 4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseNetConfig(tc.fields)
			if err == nil {
				t.Fatalf("parseNetConfig(%q) accepted it", tc.fields)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not name the bad field (want %q)", err, tc.wantWord)
			}
		})
	}
}

func TestParseNetConfig_EmptyDeviceIsNotSilentlyEth0(t *testing.T) {
	if c, err := parseNetConfig([]string{"192.168.0.7/16", "192.168.30.1", "aa:fc:c0:a8:00:07", ""}); err == nil {
		t.Fatalf("an empty device was accepted and became %q", c.Device)
	}
}

func TestHandleCredCommand_SetNet(t *testing.T) {
	hashes := newHashStore("")

	var applied []netConfig
	var applyErr error
	orig := applyNetConfig
	applyNetConfig = func(c netConfig) error {
		applied = append(applied, c)
		return applyErr
	}
	t.Cleanup(func() { applyNetConfig = orig })

	if got := handleCredCommand(theWireLine+"\n", hashes); got != "OK" {
		t.Fatalf("handleCredCommand(%q) = %q, want OK", theWireLine, got)
	}
	if len(applied) != 1 {
		t.Fatalf("the applier was called %d times, want 1 — an OK that applied nothing is the "+
			"failure this test exists for", len(applied))
	}
	if applied[0].Addr.IP.String() != "192.168.0.7" || applied[0].MAC.String() != "aa:fc:c0:a8:00:07" {
		t.Errorf("the applier got %v", applied[0])
	}

	// The node bridges this VM's tap on the strength of this reply, so a refusal must be ERR.
	applyErr = errors.New("no interface eth0")
	got := handleCredCommand(theWireLine, hashes)
	if !strings.HasPrefix(got, "ERR") {
		t.Fatalf("a failing applier still replied %q", got)
	}
	if !strings.Contains(got, "no interface eth0") {
		t.Errorf("reply %q drops the applier's reason", got)
	}
	applyErr = nil

	before := len(applied)
	if got := handleCredCommand("SETNET nonsense", hashes); !strings.HasPrefix(got, "ERR") {
		t.Fatalf("malformed SETNET replied %q", got)
	}
	if len(applied) != before {
		t.Error("a malformed SETNET reached the applier")
	}

	// SETNET must not disturb the credential store.
	if hashes.Get() != "" {
		t.Errorf("SETNET set a credential: %q", hashes.Get())
	}
	valid := strings.Repeat("a", 64)
	if got := handleCredCommand("SETHASH "+valid, hashes); got != "OK" {
		t.Fatalf("SETHASH after adding SETNET: %q", got)
	}
	if hashes.Get() != valid {
		t.Errorf("hash = %q", hashes.Get())
	}

	if got := handleCredCommand("SETWHAT x", hashes); got != "ERR unknown command" {
		t.Errorf("unknown command reply = %q", got)
	}
}

// The channel reader is a LimitReader, so an over-long line becomes a silent parse error.
func TestSetNetLineFitsTheChannel(t *testing.T) {
	longest := "SETNET 192.168.255.254/16 192.168.255.254 aa:fc:c0:a8:ff:fe " +
		strings.Repeat("e", maxIfaceName) + "\n"
	if len(longest) > credMaxLine {
		t.Fatalf("the widest SETNET line is %d bytes and credMaxLine is %d — "+
			"the channel would truncate it", len(longest), credMaxLine)
	}
}

// A stub left wired in by a test that forgot its cleanup would make every SETNET reply OK.
func TestApplyNetConfigDefaultIsTheRealApplier(t *testing.T) {
	c := netConfig{
		Addr:    &net.IPNet{IP: net.IPv4(192, 168, 0, 7), Mask: net.CIDRMask(16, 32)},
		Gateway: net.IPv4(192, 168, 30, 1),
		MAC:     net.HardwareAddr{0xaa, 0xfc, 0xc0, 0xa8, 0x00, 0x07},
		Device:  "definitelynotad",
	}
	if err := applyNetConfig(c); err == nil {
		t.Fatal("applyNetConfig succeeded against an interface that does not exist; " +
			"the applier is stubbed out")
	}
}
