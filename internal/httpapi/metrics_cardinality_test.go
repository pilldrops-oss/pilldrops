package httpapi

import (
	"fmt"
	"testing"
)

// Function names are customer input, so per-name series must be capped.
func TestFunctionLabelCardinalityIsBounded(t *testing.T) {
	m := newMetrics()
	const attempts = maxFunctionLabels * 3
	for i := range attempts {
		m.incFunctionInvoke(fmt.Sprintf("fn-%d", i))
	}

	m.mu.Lock()
	distinct := len(m.funcInvokes)
	var total int64
	for _, v := range m.funcInvokes {
		total += v
	}
	overflow := m.funcInvokes[otherLabel]
	m.mu.Unlock()

	if distinct > maxFunctionLabels+1 { // +1 for the overflow bucket itself
		t.Errorf("%d distinct label values, want at most %d", distinct, maxFunctionLabels+1)
	}
	if total != attempts {
		t.Errorf("total = %d, want %d — overflow must be folded in, not dropped", total, attempts)
	}
	if overflow == 0 {
		t.Error("want the overflow bucket to have absorbed the excess")
	}
}

func TestKnownFunctionKeepsItsOwnSeries(t *testing.T) {
	m := newMetrics()
	m.incFunctionInvoke("established")
	for i := range maxFunctionLabels * 2 {
		m.incFunctionInvoke(fmt.Sprintf("burst-%d", i))
	}
	m.incFunctionInvoke("established")

	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.funcInvokes["established"]; got != 2 {
		t.Errorf("established function count = %d, want 2", got)
	}
}
