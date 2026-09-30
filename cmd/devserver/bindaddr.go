package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
)

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

// listenLocal listens on addr and, when addr is IPv4 loopback, on IPv6 loopback
// too. A browser tries localhost as ::1 first, so without the second socket
// another program on [::1]:port answers for this server, silently. No IPv6
// loopback on the host is fine; the port being taken there is not.
func listenLocal(ctx context.Context, addr string) ([]net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return []net.Listener{ln}, nil
	}
	ln6, err := lc.Listen(ctx, "tcp", net.JoinHostPort("::1", port))
	switch {
	case err == nil:
		return []net.Listener{ln, ln6}, nil
	case errors.Is(err, syscall.EADDRINUSE):
		_ = ln.Close()
		return nil, fmt.Errorf("another program is already listening on [::1]:%s, so http://localhost:%s "+
			"would reach it instead of this server; stop it or set PORT", port, port)
	default:
		return []net.Listener{ln}, nil
	}
}
