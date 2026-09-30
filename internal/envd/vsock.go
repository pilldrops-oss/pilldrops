//go:build linux

package envd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// dialVsock dials an ip:port address over TCP, anything else as AF_VSOCK cid:port.
// Reaching a Firecracker guest over vsock would need the jailer UDS CONNECT
// handshake, which this does not implement.
func dialVsock(vsockPath string) (net.Conn, error) {
	if host, _, err := net.SplitHostPort(vsockPath); err == nil && net.ParseIP(host) != nil {
		return (&net.Dialer{}).DialContext(context.Background(), "tcp", vsockPath)
	}
	// Never fall back to a default: dialing the node's own loopback either
	// reports healthy VMs dead or runs a tenant's commands against the wrong process.
	if strings.TrimSpace(vsockPath) == "" {
		return nil, fmt.Errorf("no envd address given (empty vsockPath); refusing to dial a default")
	}
	conn, err := dialVsockRaw(vsockPath)
	if err == nil {
		return conn, nil
	}
	return nil, fmt.Errorf("vsock dial %q failed: %w", vsockPath, err)
}

func dialVsockRaw(vsockPath string) (net.Conn, error) {
	cid := uint32(3)
	port := uint32(6001)

	if parts := strings.SplitN(vsockPath, ":", 2); len(parts) == 2 {
		if c, err := strconv.ParseUint(parts[0], 10, 32); err == nil {
			cid = uint32(c)
		}
		if p, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
			port = uint32(p)
		}
	}

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}

	sa := &unix.SockaddrVM{
		CID:  cid,
		Port: port,
	}
	if err := unix.Connect(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock connect: %w", err)
	}

	file := os.NewFile(uintptr(fd), "vsock-client")
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("vsock file conn: %w", err)
	}

	return conn, nil
}
