//go:build !linux

package main

import (
	"fmt"
	"net"
)

// listenVsock always fails off Linux; those builds depend on the boot-time hash.
func listenVsock(port int) (net.Listener, error) {
	return nil, fmt.Errorf("vsock not available on this platform")
}

// localVsockCID reports the host id off Linux so credChannelAllowed refuses.
func localVsockCID() (uint32, error) {
	return unix_VMADDR_CID_HOST, fmt.Errorf("vsock not available on this platform")
}
