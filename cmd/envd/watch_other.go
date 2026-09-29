//go:build !linux

package main

import (
	"fmt"
	"io"
	"os"
)

// runWatch needs inotify; envd only ever runs inside a Linux guest.
func runWatch(_ []string, _ io.Writer) int {
	fmt.Fprintln(os.Stderr, "envd watch: needs Linux inotify")
	return 1
}
