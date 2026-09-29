package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Voice notes are transcribed inside the workstation with the provider key
// already there. The audio is written into the sandbox rather than fetched by
// it, because Telegram's download URL embeds the bot token shared by every customer.

const (
	// maxVoiceSeconds bounds what gets transcribed.
	maxVoiceSeconds = 300
	// maxVoiceBytes bounds the download regardless of the declared duration.
	maxVoiceBytes = 8 << 20
	// voicePath is where the note lands inside the sandbox; overwritten each time so recordings do not accumulate.
	voicePath = "/tmp/pd-voice-note.ogg"
)

// FileFetcher resolves a Telegram file id to a download URL.
type FileFetcher interface {
	FileURL(ctx context.Context, fileID string) (string, error)
}

// FileWriter puts bytes inside a sandbox.
type FileWriter interface {
	WriteFile(ctx context.Context, sandboxID, path string, data []byte) error
}

// Transcriber turns a Telegram voice note into text using the workstation's own
// provider key.
type Transcriber struct {
	Files  FileFetcher
	Writer FileWriter
	Boxes  Sandboxes
	HTTP   *http.Client
}

// ErrNoVoiceKey reports that the workstation has no key to transcribe with. Shown verbatim to the user.
var ErrNoVoiceKey = fmt.Errorf(
	"voice needs an OpenAI-compatible key on the workstation — relaunch it with OPENAI_API_KEY set, or send the task as text")

// Transcribe downloads a voice note, places it in the sandbox and transcribes it there.
func (t *Transcriber) Transcribe(ctx context.Context, sandboxID, fileID string, durationSecs int) (string, error) {
	if durationSecs > maxVoiceSeconds {
		return "", fmt.Errorf("that note is %ds — keep it under %ds", durationSecs, maxVoiceSeconds)
	}
	url, err := t.Files.FileURL(ctx, fileID)
	if err != nil {
		return "", fmt.Errorf("couldn't fetch that note: %w", err)
	}
	audio, err := t.download(ctx, url)
	if err != nil {
		return "", err
	}
	if err := t.Writer.WriteFile(ctx, sandboxID, voicePath, audio); err != nil {
		return "", fmt.Errorf("couldn't put the note on your workstation: %w", err)
	}

	res, err := t.Boxes.Exec(ctx, sandboxID, rt.ExecSpec{
		Cmd: transcribeScript(), Timeout: 2 * time.Minute,
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode == 2 {
		return "", ErrNoVoiceKey
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = out
		}
		return "", fmt.Errorf("transcription failed: %s", Truncate(detail, 300))
	}
	if out == "" {
		return "", fmt.Errorf("that note came back empty — try again, or send it as text")
	}
	return out, nil
}

func (t *Transcriber) download(ctx context.Context, url string) ([]byte, error) {
	c := t.HTTP
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't download that note: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("couldn't download that note: %s", resp.Status)
	}
	// A declared Content-Length is only a claim; the LimitReader is the real bound.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVoiceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxVoiceBytes {
		return nil, fmt.Errorf("that note is too large to transcribe")
	}
	return data, nil
}

// transcribeScript runs inside the sandbox against its own provider key.
// Exit 2 means no key configured, distinct from a transcription failure.
func transcribeScript() string {
	return `
set -u
if [ -z "${OPENAI_API_KEY:-}" ]; then exit 2; fi
base="${OPENAI_BASE_URL:-https://api.openai.com/v1}"
out=$(curl -sS --max-time 100 -X POST "$base/audio/transcriptions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -F "model=${PD_VOICE_MODEL:-whisper-1}" \
  -F "response_format=text" \
  -F "file=@` + voicePath + `") || exit 1
rm -f ` + voicePath + `
printf '%s' "$out"
`
}
