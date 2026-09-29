package parse

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// testdata/claude-opus-4-7-live.jsonl is a verbatim StreamCmd capture on a second
// model, so these tests check the frame contract is not model-dependent.
func liveOpus47Steps(t *testing.T) []Step {
	t.Helper()
	f, err := os.Open("testdata/claude-opus-4-7-live.jsonl")
	if err != nil {
		t.Fatalf("open the live capture: %v", err)
	}
	defer f.Close()

	p := For(FormatClaude)
	var steps []Step
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		steps = append(steps, p.Parse(sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(steps) < 5 {
		t.Fatalf("only %d frames — the capture is truncated, so this test is not checking "+
			"what it claims to", len(steps))
	}
	return steps
}

func TestOpus47FrameContractIsUnchanged(t *testing.T) {
	kinds := map[Kind]int{}
	for i, s := range liveOpus47Steps(t) {
		if s.Kind == KindUnknown {
			t.Errorf("frame %d from claude-opus-4-7 parsed as unknown: the stream shape is "+
				"model-dependent, so switching models empties the timeline", i)
		}
		kinds[s.Kind]++
	}
	for _, k := range []Kind{KindSession, KindThinking, KindToolStart, KindToolEnd, KindText, KindResult} {
		if kinds[k] == 0 {
			t.Errorf("no %s step on claude-opus-4-7, though the run demonstrably produced one", k)
		}
	}
}

func TestOpus47CarriesCostAndTokens(t *testing.T) {
	steps := liveOpus47Steps(t)
	var result *Step
	for i := range steps {
		if steps[i].Kind == KindResult {
			result = &steps[i]
		}
	}
	if result == nil {
		t.Fatal("no result frame: the turn would record neither an answer nor a cost")
	}
	if result.Usage.CostUSD == nil {
		t.Fatal("total_cost_usd absent on the new model — the spend cap would count every " +
			"turn as free")
	}
	if *result.Usage.CostUSD <= 0 {
		t.Errorf("cost = %v from a genuinely billed call", *result.Usage.CostUSD)
	}
	if result.Usage.OutputTokens <= 0 {
		t.Errorf("output tokens = %d", result.Usage.OutputTokens)
	}
	// This run was a cache creation, so cache reads are legitimately zero.
	if result.Usage.InputTokens <= 0 && result.Usage.CacheReadTokens <= 0 {
		t.Error("neither input nor cache-read tokens counted, so the turn looks like it read nothing")
	}
	if result.Text == "" {
		t.Error("no final answer text on the result frame")
	}
}

// The result frame also carries session_id, but the init frame arrives first, so
// mid-turn events can be attributed to a thread.
func TestOpus47SessionIDComesFromTheInitFrame(t *testing.T) {
	var session *Step
	for _, s := range liveOpus47Steps(t) {
		if s.Kind == KindSession {
			c := s
			session = &c
			break
		}
	}
	if session == nil {
		t.Fatal("no session frame")
	}
	if session.SessionID == "" {
		t.Error("no session id on the init frame, so threads cannot be derived from our records")
	}
	if session.Model == "" {
		t.Error("no model on the init frame — status could not report what is actually running")
	}
}
