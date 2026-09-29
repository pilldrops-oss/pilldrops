package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// FeedbackItem is one contact-form submission.
type FeedbackItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Source  string `json:"source"`
	IP      string `json:"ip"`
	TS      int64  `json:"ts"` // unix seconds
}

type feedbackStore struct {
	mu   sync.Mutex
	max  int
	list []FeedbackItem
}

func newFeedbackStore(max int) *feedbackStore { return &feedbackStore{max: max} }

func (f *feedbackStore) add(it FeedbackItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, it)
	if len(f.list) > f.max {
		f.list = f.list[len(f.list)-f.max:]
	}
}

// all returns a copy of the buffer, newest first.
func (f *feedbackStore) all() []FeedbackItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FeedbackItem, len(f.list))
	for i, it := range f.list {
		out[len(f.list)-1-i] = it
	}
	return out
}

func (s *Server) handleFeedbackCreate(w http.ResponseWriter, r *http.Request) {
	var in FeedbackItem
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	in.Message = strings.TrimSpace(in.Message)
	if in.Message == "" || len(in.Message) > 8000 {
		writeErr(w, 400, "message required")
		return
	}
	now := s.now()
	in.TS = now.Unix()
	in.ID = "fb_" + strconv.FormatInt(now.UnixNano(), 36)
	if strings.TrimSpace(in.Kind) == "" {
		in.Kind = "feedback"
	}
	if strings.TrimSpace(in.Source) == "" {
		in.Source = "portal"
	}
	s.fb.add(in)
	s.log().InfoContext(r.Context(), "feedback received", "component", "feedback",
		"id", in.ID, "kind", in.Kind, "from", in.Name, "email", in.Email, "message", in.Message)
	writeJSON(w, 200, map[string]any{"ok": true, "id": in.ID})
}

func (s *Server) handleFeedbackList(w http.ResponseWriter, r *http.Request) {
	items := s.fb.all()
	writeJSON(w, 200, map[string]any{"feedback": items, "count": len(items)})
}
