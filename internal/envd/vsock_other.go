//go:build !linux

package envd

import "net"

// dialVsock on non-Linux ignores the address and dials the dev-mode TCP port.
func dialVsock(vsockPath string) (net.Conn, error) {
	return net.Dial("tcp", "localhost:6001")
}
