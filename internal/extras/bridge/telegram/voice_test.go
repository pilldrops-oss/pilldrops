package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

type fakeFiles struct {
	url string
	err error
}

func (f *fakeFiles) FileURL(context.Context, string) (string, error) { return f.url, f.err }

type fakeWriter struct {
	path string
	data []byte
	err  error
}

func (w *fakeWriter) WriteFile(_ context.Context, _, path string, data []byte) error {
	w.path, w.data = path, data
	return w.err
}

func newAudioServer(t *testing.T, body []byte, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTranscribeHappyPath(t *testing.T) {
	url := newAudioServer(t, []byte("OggS-fake-audio"), 200)
	boxes := &fakeBoxes{exists: true, state: SandboxState{Template: "claude"},
		res: rt.ExecResult{Stdout: "  update the changelog and open a PR \n"}}
	w := &fakeWriter{}
	tr := &Transcriber{Files: &fakeFiles{url: url}, Writer: w, Boxes: boxes}

	got, err := tr.Transcribe(context.Background(), "sb_1", "f1", 8)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if got != "update the changelog and open a PR" {
		t.Errorf("transcript = %q", got)
	}
	if string(w.data) != "OggS-fake-audio" {
		t.Errorf("audio not written to the sandbox: %q", w.data)
	}
	if w.path != voicePath {
		t.Errorf("wrote to %q, want %q", w.path, voicePath)
	}
}

func TestTranscribeNoKeyIsDistinct(t *testing.T) {
	url := newAudioServer(t, []byte("audio"), 200)
	boxes := &fakeBoxes{exists: true, res: rt.ExecResult{ExitCode: 2}}
	tr := &Transcriber{Files: &fakeFiles{url: url}, Writer: &fakeWriter{}, Boxes: boxes}

	_, err := tr.Transcribe(context.Background(), "sb_1", "f1", 5)
	if !errors.Is(err, ErrNoVoiceKey) {
		t.Errorf("error = %v, want ErrNoVoiceKey", err)
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("error does not name the fix: %v", err)
	}
}

func TestTranscribeRejectsLongNotes(t *testing.T) {
	tr := &Transcriber{Files: &fakeFiles{}, Writer: &fakeWriter{}, Boxes: &fakeBoxes{}}
	if _, err := tr.Transcribe(context.Background(), "sb_1", "f1", maxVoiceSeconds+1); err == nil {
		t.Error("accepted a note past the duration cap")
	}
}

func TestTranscribeRejectsOversizeAudio(t *testing.T) {
	url := newAudioServer(t, make([]byte, maxVoiceBytes+10), 200)
	tr := &Transcriber{Files: &fakeFiles{url: url}, Writer: &fakeWriter{}, Boxes: &fakeBoxes{}}
	if _, err := tr.Transcribe(context.Background(), "sb_1", "f1", 5); err == nil {
		t.Error("accepted audio past the size cap")
	}
}

func TestTranscribeDownloadFailure(t *testing.T) {
	url := newAudioServer(t, nil, 500)
	tr := &Transcriber{Files: &fakeFiles{url: url}, Writer: &fakeWriter{}, Boxes: &fakeBoxes{}}
	if _, err := tr.Transcribe(context.Background(), "sb_1", "f1", 5); err == nil {
		t.Error("a failed download was treated as success")
	}
}

func TestTranscribeEmptyResultIsAnError(t *testing.T) {
	url := newAudioServer(t, []byte("audio"), 200)
	boxes := &fakeBoxes{exists: true, res: rt.ExecResult{Stdout: "   "}}
	tr := &Transcriber{Files: &fakeFiles{url: url}, Writer: &fakeWriter{}, Boxes: boxes}
	if _, err := tr.Transcribe(context.Background(), "sb_1", "f1", 5); err == nil {
		t.Error("an empty transcript was accepted as a task")
	}
}

// Telegram's download URL embeds the bot token, which must never reach a sandbox.
func TestTranscribeScriptCarriesNoBotToken(t *testing.T) {
	script := transcribeScript()
	for _, bad := range []string{"api.telegram.org", "/bot", "getFile"} {
		if strings.Contains(script, bad) {
			t.Errorf("the in-sandbox script references Telegram (%q) — the bot token must never reach a customer's shell:\n%s", bad, script)
		}
	}
	if !strings.Contains(script, "OPENAI_API_KEY") {
		t.Error("script does not use the workstation's own key")
	}
	if !strings.Contains(script, "exit 2") {
		t.Error("script has no distinct no-key exit code")
	}
	if !strings.Contains(script, "rm -f "+voicePath) {
		t.Error("script leaves the recording on disk")
	}
}
