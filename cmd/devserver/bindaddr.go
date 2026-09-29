package main

import "net"

// safeBindAddr rewrites a wildcard bind to loopback when /v1 accepts anonymous
// callers (no portal key), unless the operator opts in to public access.
func safeBindAddr(listenAddr string, authEnforced, allowPublicAnon bool) (string, bool) {
	if authEnforced {
		return listenAddr, false
	}
	if allowPublicAnon {
		return listenAddr, true
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr, false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("127.0.0.1", port), false
	}
	return listenAddr, false
}
