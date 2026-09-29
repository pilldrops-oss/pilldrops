package docker

import (
	"strings"
	"testing"
)

func TestFileCapVerdictReadsReachingTheCeilingAsUnbounded(t *testing.T) {
	bounded, detail := fileCapVerdict(200000, 200000)
	if bounded {
		t.Error("reaching the ceiling means NOTHING stopped the container — that is the " +
			"finding, not a pass")
	}
	if !strings.Contains(detail, "inode") {
		t.Errorf("the detail does not name the consequence, so an operator reading it in a "+
			"boot log has to already know what an unbounded file count does: %q", detail)
	}
}

func TestFileCapVerdictReadsStoppingShortAsBounded(t *testing.T) {
	for _, created := range []int{0, 1, 131072, 199999} {
		if bounded, _ := fileCapVerdict(created, 200000); !bounded {
			t.Errorf("created=%d stopped short of the ceiling and was still called unbounded", created)
		}
	}
}

// ls | wc -l can report more than requested if anything else lands in the probe dir.
func TestFileCapVerdictTreatsOvershootAsUnbounded(t *testing.T) {
	if bounded, _ := fileCapVerdict(200001, 200000); bounded {
		t.Error("a count ABOVE the ceiling was read as bounded")
	}
}
