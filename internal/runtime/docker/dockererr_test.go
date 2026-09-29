package docker

import (
	"errors"
	"strings"
	"testing"
)

// dockerOut reports a non-zero exit with err nil, so an unconditional %w printed "%!w(<nil>)".
func TestDockerErrDoesNotPrintAFormattingArtefact(t *testing.T) {
	err := dockerErr("docker port abc123", nil, 1,
		"no public port '8000/tcp' published for abc123\n")

	if strings.Contains(err.Error(), "%!w") {
		t.Fatalf("still emitting a verb artefact: %q", err)
	}
	if !strings.Contains(err.Error(), "no public port '8000/tcp' published") {
		t.Fatalf("dropped the reason docker gave: %q", err)
	}
	if !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("no exit status, so a silent non-zero exit is indistinguishable from success: %q", err)
	}
}

func TestDockerErrWrapsARealError(t *testing.T) {
	sentinel := errors.New("exec: \"docker\": executable file not found in $PATH")
	err := dockerErr("docker run img", sentinel, 0, "")

	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is lost the cause: %q", err)
	}
	if strings.Contains(err.Error(), "exit 0") {
		t.Fatalf("reported an exit status for a command that never ran: %q", err)
	}
}

func TestDockerErrKeepsBothCauseAndStderr(t *testing.T) {
	sentinel := errors.New("signal: killed")
	err := dockerErr("docker run img", sentinel, 0, "  oom  ")

	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is lost the cause: %q", err)
	}
	if !strings.Contains(err.Error(), "oom") {
		t.Fatalf("dropped stderr: %q", err)
	}
}

func TestDockerErrWithNoStderrStillNamesTheFailure(t *testing.T) {
	err := dockerErr("docker port abc", nil, 125, "")
	if got := err.Error(); got != "docker port abc: exit 125" {
		t.Fatalf("got %q, want \"docker port abc: exit 125\"", got)
	}
}
