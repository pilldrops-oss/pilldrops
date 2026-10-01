package docker

import (
	"os"
	"strconv"
	"testing"
	"time"
)

func TestProcStartedReadsTheProcessStartTime(t *testing.T) {
	got := procStarted(strconv.Itoa(os.Getpid()))
	if got.IsZero() || time.Since(got) < 0 || time.Since(got) > 10*time.Minute {
		t.Errorf("this test process started at %v, want within the last few minutes", got)
	}
	if !procStarted("not-a-pid").IsZero() {
		t.Error("a missing process has a start time")
	}
}
