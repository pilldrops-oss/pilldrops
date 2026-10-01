//go:build !linux

package docker

import "time"

// dockerdStarted is unknown off Linux, where dockerd runs in a VM.
func dockerdStarted() time.Time { return time.Time{} }
