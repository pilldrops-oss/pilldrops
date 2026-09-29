package agentd

import (
	"os"
	"path/filepath"
	"testing"
)

// The agent can write to the journal file, so Replay parses attacker-influenced
// bytes. It must never panic and never report seqs out of order, since the mirror
// derives its cursor from the max seq and would skip everything after a fabricated one.
func FuzzJournalReplay(f *testing.F) {
	f.Add(`{"seq":1,"at":"2026-01-01T00:00:00Z","type":"turn.started"}`)
	f.Add(`{"seq":1,"type":"a"}` + "\n" + `{"seq":2,"type":"b"}`)
	f.Add(`{"seq":2,"type":"b"}` + "\n" + `{"seq":1,"type":"a"}`) // out of order
	f.Add(`{"seq":99999999999999999999,"type":"huge"}`)
	f.Add(`{"seq":-1,"type":"negative"}`)
	f.Add("not json\n{\"seq\":1,\"type\":\"after junk\"}")
	f.Add("")
	f.Add("\n\n\n")
	f.Add(`{"seq":1,"type":"torn"`) // torn tail

	f.Fuzz(func(t *testing.T, contents string) {
		dir := t.TempDir()
		// Written directly, bypassing Append, as a tampered file would be.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Skip()
		}
		path := filepath.Join(dir, "events.jsonl")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Skip()
		}

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("journal replay panicked on %q: %v", trunc(contents), r)
			}
		}()

		j, err := OpenJournal(dir, nil)
		if err != nil {
			return // refusing a corrupt journal is valid
		}
		defer j.Close()

		var last uint64
		err = j.Replay(0, func(seq uint64, line []byte) error {
			if seq <= last && last != 0 {
				t.Fatalf("replay produced seq %d after %d for %q — the mirror would skip "+
					"everything after the higher value", seq, last, trunc(contents))
			}
			last = seq
			if len(line) == 0 {
				t.Fatalf("replay produced an empty line for %q", trunc(contents))
			}
			return nil
		})
		_ = err // a read error is valid; a panic or bad ordering is not
	})
}

func trunc(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
