package httpapi

import (
	"net/http"
	"testing"
)

type flushProbe struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushProbe) Flush() { f.flushed = true }

// The SSE /exec/stream handler asserts w.(http.Flusher) through this wrapper.
func TestStatusRecorderFlusher(t *testing.T) {
	var _ http.Flusher = (*statusRecorder)(nil)
	probe := &flushProbe{}
	rec := &statusRecorder{ResponseWriter: probe}
	fl, ok := interface{}(rec).(http.Flusher)
	if !ok {
		t.Fatal("statusRecorder must implement http.Flusher")
	}
	fl.Flush()
	if !probe.flushed {
		t.Fatal("statusRecorder.Flush did not delegate to the underlying writer")
	}
}
