//go:build !linux

package main

import "errors"

// setHostname refuses off Linux, rather than letting the node believe a child
// was renamed when nothing touched it.
func setHostname(string) error {
	return errors.New("this build cannot set the guest hostname")
}
