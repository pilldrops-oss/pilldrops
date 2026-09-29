package agentd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The journal is the source of truth. It is append-only JSON Lines on the
// sandbox's own disk, so it rides the pause/resume snapshot.

const (
	// maxOutputPerTurn caps captured output so a runaway turn cannot fill the disk.
	maxOutputPerTurn = 8 << 20 // 8 MiB

	// maxSegmentBytes rotates the journal so recovery scans one file and retention drops whole files.
	maxSegmentBytes = 16 << 20 // 16 MiB

	segmentExt = ".jsonl"

	// maxSeq bounds a plausible sequence number. The file is corruption-reachable,
	// and a seq near 2^64 would wrap the counter to zero on the next append.
	maxSeq = uint64(1) << 56
)

// Journal is an append-only event log with monotonic sequence numbers.
type Journal struct {
	dir string
	red *Redactor
	now func() time.Time
	// maxOutput is a field so tests can exercise truncation cheaply.
	maxOutput int64

	mu      sync.Mutex
	f       *os.File
	segment int
	size    int64
	seq     uint64
	// outBytes and truncated are per-turn and cleared by FinishTurn.
	outBytes  map[string]int64
	truncated map[string]bool
	// subs are streaming clients waiting for a nudge.
	subs map[chan struct{}]struct{}
}

// OpenJournal opens or creates the journal in dir and recovers the sequence counter.
func OpenJournal(dir string, red *Redactor) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal dir: %w", err)
	}
	j := &Journal{
		dir: dir, red: red, now: time.Now, maxOutput: maxOutputPerTurn,
		outBytes: map[string]int64{}, truncated: map[string]bool{},
	}
	if err := j.recover(); err != nil {
		return nil, err
	}
	return j, nil
}

// recover finds the newest segment, truncates a torn final line, and restores
// the sequence counter.
func (j *Journal) recover() error {
	segs, err := j.segments()
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return j.openSegment(1)
	}

	last := segs[len(segs)-1]
	path := filepath.Join(j.dir, segmentName(last))
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open segment %d: %w", last, err)
	}
	var (
		highest   uint64
		goodBytes int64
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxOutputPerTurn+1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// Treat everything from here on as torn.
			break
		}
		if probe.Seq > maxSeq {
			break
		}
		if probe.Seq > highest {
			highest = probe.Seq
		}
		goodBytes += int64(len(line)) + 1 // + newline
	}
	scanErr := sc.Err()
	_ = f.Close()
	if scanErr != nil && !errors.Is(scanErr, io.EOF) && !isTokenTooLong(scanErr) {
		return fmt.Errorf("scan segment %d: %w", last, scanErr)
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != goodBytes {
		if err := os.Truncate(path, goodBytes); err != nil {
			return fmt.Errorf("repair torn segment %d: %w", last, err)
		}
	}
	j.seq = highest

	if goodBytes >= maxSegmentBytes {
		return j.openSegment(last + 1)
	}
	return j.reopenSegment(last, goodBytes)
}

// isTokenTooLong reports an oversized line, treated as a torn tail rather than a
// hard error so one bad record does not take the agent down.
func isTokenTooLong(err error) bool {
	return err != nil && strings.Contains(err.Error(), "token too long")
}

func (j *Journal) segments() ([]int, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), segmentExt) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(e.Name(), segmentExt))
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

func segmentName(n int) string { return fmt.Sprintf("%06d%s", n, segmentExt) }

func (j *Journal) openSegment(n int) error {
	f, err := os.OpenFile(filepath.Join(j.dir, segmentName(n)),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open segment %d: %w", n, err)
	}
	j.f, j.segment, j.size = f, n, 0
	return nil
}

func (j *Journal) reopenSegment(n int, size int64) error {
	f, err := os.OpenFile(filepath.Join(j.dir, segmentName(n)),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen segment %d: %w", n, err)
	}
	j.f, j.segment, j.size = f, n, size
	return nil
}

// LastSeq is the highest sequence number written.
func (j *Journal) LastSeq() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq
}

// Subscribe returns a channel nudged on every append, plus a release function.
// Subscribe before reading the head so no event is missed; nudges are coalesced.
func (j *Journal) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	j.mu.Lock()
	if j.subs == nil {
		j.subs = map[chan struct{}]struct{}{}
	}
	j.subs[ch] = struct{}{}
	j.mu.Unlock()

	return ch, func() {
		j.mu.Lock()
		delete(j.subs, ch)
		j.mu.Unlock()
	}
}

// notifyLocked nudges every subscriber without blocking. Caller holds j.mu.
func (j *Journal) notifyLocked() {
	for ch := range j.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Append writes one event and returns it with Seq and At filled in.
func (j *Journal) Append(typ, turnID string, payload map[string]any) (Event, error) {
	return j.AppendStep(typ, turnID, 0, payload)
}

// AppendStep is Append with a step index, for ordering tool calls within a turn.
func (j *Journal) AppendStep(typ, turnID string, step int, payload map[string]any) (Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(typ, turnID, step, payload)
}

func (j *Journal) appendLocked(typ, turnID string, step int, payload map[string]any) (Event, error) {
	// Redact before marshaling; JSON escaping would break multi-line patterns like PEM blocks.
	var raw json.RawMessage
	if len(payload) > 0 {
		clean := redactValue(j.red, payload)
		b, err := json.Marshal(clean)
		if err != nil {
			return Event{}, fmt.Errorf("marshal payload for %s: %w", typ, err)
		}
		raw = b
	}

	if j.seq >= maxSeq {
		return Event{}, fmt.Errorf("journal sequence exhausted at %d", j.seq)
	}
	j.seq++
	ev := Event{Seq: j.seq, At: j.now().UTC(), Type: typ, TurnID: turnID, Step: step, Payload: raw}

	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, fmt.Errorf("marshal event %s: %w", typ, err)
	}
	line = append(line, '\n')

	if j.size > 0 && j.size+int64(len(line)) > maxSegmentBytes {
		if err := j.rotateLocked(); err != nil {
			return Event{}, err
		}
	}
	n, err := j.f.Write(line)
	j.size += int64(n)
	if err != nil {
		return Event{}, fmt.Errorf("append %s: %w", typ, err)
	}
	// Notify only after the bytes are in the file.
	j.notifyLocked()
	return ev, nil
}

func (j *Journal) rotateLocked() error {
	if err := j.f.Sync(); err != nil {
		return err
	}
	if err := j.f.Close(); err != nil {
		return err
	}
	return j.openSegment(j.segment + 1)
}

// AppendOutput journals agent output for a turn, enforcing the per-turn ceiling
// with a single truncation marker.
func (j *Journal) AppendOutput(turnID, text string) (Event, error) {
	if text == "" {
		return Event{}, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.truncated[turnID] {
		return Event{}, nil
	}
	used := j.outBytes[turnID]
	room := j.maxOutput - used
	if room <= 0 {
		j.truncated[turnID] = true
		return j.appendLocked(EventTurnOutputTruncated, turnID, 0, map[string]any{
			"limit_bytes": j.maxOutput,
			"reason":      "per-turn output limit reached; further output for this turn is not recorded",
		})
	}
	if int64(len(text)) > room {
		// A split rune becomes U+FFFD in JSON, not a corrupt file.
		text = text[:room]
		j.outBytes[turnID] = j.maxOutput
		if _, err := j.appendLocked(EventTurnOutput, turnID, 0, map[string]any{"text": text}); err != nil {
			return Event{}, err
		}
		j.truncated[turnID] = true
		return j.appendLocked(EventTurnOutputTruncated, turnID, 0, map[string]any{
			"limit_bytes": j.maxOutput,
			"reason":      "per-turn output limit reached; further output for this turn is not recorded",
		})
	}
	j.outBytes[turnID] = used + int64(len(text))
	return j.appendLocked(EventTurnOutput, turnID, 0, map[string]any{"text": text})
}

// FinishTurn releases the per-turn output accounting and fsyncs. Syncing only at
// turn boundaries is fine because the data at risk between them was streamed live.
func (j *Journal) FinishTurn(turnID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.outBytes, turnID)
	delete(j.truncated, turnID)
	return j.f.Sync()
}

// Sync flushes buffered writes to disk.
func (j *Journal) Sync() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Sync()
}

// Close flushes and releases the current segment.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Sync()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	j.f = nil
	return err
}

// Replay calls fn with the stored line for every event after since, in order,
// stopping at the first error fn returns.
func (j *Journal) Replay(since uint64, fn func(seq uint64, line []byte) error) error {
	segs, err := j.segments()
	if err != nil {
		return err
	}
	for _, n := range segs {
		if err := j.replaySegment(n, since, fn); err != nil {
			return err
		}
	}
	return nil
}

func (j *Journal) replaySegment(n int, since uint64, fn func(uint64, []byte) error) error {
	f, err := os.Open(filepath.Join(j.dir, segmentName(n)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // dropped by retention mid-replay
		}
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxOutputPerTurn+1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// An unrepaired torn tail.
			return nil
		}
		if probe.Seq <= since {
			continue
		}
		// The scanner reuses its buffer.
		cp := make([]byte, len(line))
		copy(cp, line)
		if err := fn(probe.Seq, cp); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && !isTokenTooLong(err) {
		return err
	}
	return nil
}

// redactValue redacts every string in a payload at any depth without mutating the input.
func redactValue(r *Redactor, v any) any {
	switch t := v.(type) {
	case string:
		return r.Redact(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = redactValue(r, val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValue(r, val)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = r.Redact(s)
		}
		return out
	default:
		return v
	}
}
