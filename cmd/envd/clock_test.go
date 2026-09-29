package main

import (
	"strings"
	"testing"
)

// SETTIME's argument is parsed before anything touches the clock, so a mangled
// push cannot step a guest to 1970 (which would break every TLS handshake).
// The happy path is not exercised here: it steps this machine's clock.
func TestSetTimeRejectsBadArguments(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"no argument", credSetTime},
		{"two arguments", credSetTime + " 1 2"},
		{"not a number", credSetTime + " tomorrow"},
		{"zero", credSetTime + " 0"},
		{"negative", credSetTime + " -1000000000"},
		{"overflows int64", credSetTime + " 99999999999999999999999"},
		{"float", credSetTime + " 1.5e18"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := handleCredCommand(tc.line+"\n", newHashStore(""))
			if !strings.HasPrefix(reply, "ERR") {
				t.Fatalf("reply = %q, want an ERR — this argument must never reach clock_settime", reply)
			}
		})
	}
	// Control: the command itself is known, so the refusals above are the
	// argument check and not "unknown command".
	if reply := handleCredCommand(credSetTime+" 1700000000000000000 extra\n", newHashStore("")); !strings.Contains(reply, credSetTime) {
		t.Fatalf("reply = %q, want the arity error naming %s", reply, credSetTime)
	}
}
