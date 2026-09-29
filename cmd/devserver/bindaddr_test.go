package main

import "testing"

func TestSafeBindAddr(t *testing.T) {
	cases := []struct {
		name         string
		listen       string
		authEnforced bool
		allowPublic  bool
		wantAddr     string
		wantWarn     bool
	}{
		{"anon+wildcard -> loopback", ":8080", false, false, "127.0.0.1:8080", false},
		{"auth enforced keeps wildcard", ":8080", true, false, ":8080", false},
		{"explicit public-anon opt-in warns", ":8080", false, true, ":8080", true},
		{"already loopback unchanged", "127.0.0.1:9000", false, false, "127.0.0.1:9000", false},
		{"explicit 0.0.0.0 anon -> loopback", "0.0.0.0:8080", false, false, "127.0.0.1:8080", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, warn := safeBindAddr(c.listen, c.authEnforced, c.allowPublic)
			if addr != c.wantAddr || warn != c.wantWarn {
				t.Fatalf("safeBindAddr(%q,%v,%v) = (%q,%v), want (%q,%v)",
					c.listen, c.authEnforced, c.allowPublic, addr, warn, c.wantAddr, c.wantWarn)
			}
		})
	}
}
