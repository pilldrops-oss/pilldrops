package main

import (
	"encoding/json"
	"io"
)

// watchEvent is one line of `envd watch` output. Op is create, write, remove,
// rename or chmod; the first line is op "ready", once the watch is in place, so
// a caller never misses a change made right after it asked.
type watchEvent struct {
	Op   string `json:"op"`
	Name string `json:"name,omitempty"`
}

// emitWatch writes one event as a JSON line. A failed write means the reader is
// gone, which ends the watch.
func emitWatch(w io.Writer, op, name string) error {
	return json.NewEncoder(w).Encode(watchEvent{Op: op, Name: name})
}
