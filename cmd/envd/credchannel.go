package main

import (
	"fmt"
	"net"
	"os"
)

// startCredChannel opens this sandbox's credential channel, or returns an error
// saying why there is none. Docker and non-Linux builds hit the error on every
// boot by design; main reports it in the transport notice.
func startCredChannel(port int, hashes *hashStore) (net.Listener, error) {
	cid, err := localVsockCID()
	if err != nil {
		return nil, fmt.Errorf("no local vsock context id: %w", err)
	}
	if ok, why := credChannelAllowed(os.Getenv("ENVD_TOKEN_HASH"), cid); !ok {
		return nil, fmt.Errorf("refused: %s", why)
	}
	l, err := listenVsock(port)
	if err != nil {
		return nil, err
	}
	go serveCredChannel(l, hashes)
	return l, nil
}
