package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// LogBroadcaster fans out log lines to connected dashboard log streams.
type LogBroadcaster struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{subs: map[chan []byte]struct{}{}}
}

func (b *LogBroadcaster) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}
func (b *LogBroadcaster) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.subs, ch)
	close(ch)
	b.mu.Unlock()
}

// Publish sends a log line to all subscribers. Non-blocking.
func (b *LogBroadcaster) Publish(level, msg string) {
	data, _ := json.Marshal(map[string]any{
		"level": level, "time": time.Now().Format(time.RFC3339), "msg": msg, "fields": map[string]any{},
	})
	b.mu.Lock()
	for ch := range b.subs {
		select {
		case ch <- data:
		default:
		}
	}
	b.mu.Unlock()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	send := func() {
		data, _ := json.Marshal(s.statsSnapshot())
		fmt.Fprintf(w, "event: stats\ndata: %s\n\n", data)
		f.Flush()
	}
	send()
	for {
		select {
		case <-ticker.C:
			send()
		case <-r.Context().Done():
			return
		case <-s.done:
			return // release the connection so Shutdown can drain
		}
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := s.logs.subscribe()
	defer s.logs.unsubscribe(ch)
	f.Flush()
	for {
		select {
		case data := <-ch:
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			f.Flush()
		case <-r.Context().Done():
			return
		case <-s.done:
			return // release the connection so Shutdown can drain
		}
	}
}
