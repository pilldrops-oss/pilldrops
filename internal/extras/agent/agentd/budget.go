package agentd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
)

// spendThresholds are the cap percentages at which a user is warned.
var spendThresholds = []int{50, 80, 100}

// BudgetSnapshot is the reportable state of a cap.
type BudgetSnapshot struct {
	// CapUSD is 0 when no cap is set, meaning unlimited.
	CapUSD   float64 `json:"cap_usd"`
	SpentUSD float64 `json:"spent_usd"`
	// RemainingUSD is nil when there is no cap.
	RemainingUSD *float64 `json:"remaining_usd,omitempty"`
	Exceeded     bool     `json:"exceeded"`
	// TurnsUnreported counts turns that produced no cost figure.
	TurnsUnreported int `json:"turns_unreported"`
	// Coverage is a one-sentence note on what the cap does not count, for a UI to show.
	Coverage string `json:"coverage"`
}

type budgetState struct {
	CapUSD          float64 `json:"cap_usd"`
	SpentUSD        float64 `json:"spent_usd"`
	TurnsUnreported int     `json:"turns_unreported"`
	// Fired records thresholds already announced, so a restart does not repeat them.
	Fired []int `json:"fired"`
}

// Budget is a durable cap on agent-reported model spend for one workstation.
// Machine time is billed separately; turns that report no cost are counted, not
// treated as zero.
type Budget struct {
	path string
	mu   sync.Mutex
	st   budgetState
}

// OpenBudget loads (or creates) the budget beside the journal and queue.
func OpenBudget(path string) (*Budget, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("budget dir: %w", err)
	}
	b := &Budget{path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		// A corrupt file starts from zero rather than locking the workstation.
		_ = json.Unmarshal(data, &b.st)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read budget: %w", err)
	}
	return b, nil
}

// saveLocked persists best effort: losing a spend update beats failing the turn.
func (b *Budget) saveLocked() {
	data, err := json.Marshal(b.st)
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, b.path)
	}
}

// SetCap sets the cap in USD; zero or negative removes it. Lowering it below what
// is already spent exceeds it immediately, which is intended.
func (b *Budget) SetCap(usd float64) BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	if usd < 0 {
		usd = 0
	}
	b.st.CapUSD = usd
	// A new cap re-arms the warnings.
	b.st.Fired = nil
	b.saveLocked()
	return b.snapshotLocked()
}

// Record accounts for one finished turn and returns the thresholds newly crossed.
// A nil cost means the agent reported nothing, counted separately rather than as zero.
func (b *Budget) Record(costUSD *float64) (crossed []int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if costUSD == nil {
		b.st.TurnsUnreported++
		b.saveLocked()
		return nil
	}
	if *costUSD > 0 {
		b.st.SpentUSD += *costUSD
	}
	if b.st.CapUSD > 0 {
		pct := b.st.SpentUSD / b.st.CapUSD * 100
		for _, t := range spendThresholds {
			if pct >= float64(t) && !slices.Contains(b.st.Fired, t) {
				b.st.Fired = append(b.st.Fired, t)
				crossed = append(crossed, t)
			}
		}
		sort.Ints(b.st.Fired)
	}
	b.saveLocked()
	return crossed
}

// Exceeded reports whether the cap is reached. Always false with no cap set.
func (b *Budget) Exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceededLocked()
}

func (b *Budget) exceededLocked() bool {
	return b.st.CapUSD > 0 && b.st.SpentUSD >= b.st.CapUSD
}

// Snapshot reports the budget for the API.
func (b *Budget) Snapshot() BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

func (b *Budget) snapshotLocked() BudgetSnapshot {
	s := BudgetSnapshot{
		CapUSD: b.st.CapUSD, SpentUSD: b.st.SpentUSD,
		Exceeded: b.exceededLocked(), TurnsUnreported: b.st.TurnsUnreported,
		Coverage: budgetCoverage,
	}
	if b.st.CapUSD > 0 {
		rem := b.st.CapUSD - b.st.SpentUSD
		if rem < 0 {
			rem = 0
		}
		s.RemainingUSD = &rem
	}
	return s
}

const budgetCoverage = "Counts model spend the agent reports. Agents that report no cost " +
	"(common with your own provider key) are counted as unreported turns, not as zero spend. " +
	"Machine time is metered and billed separately."
