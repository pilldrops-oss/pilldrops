//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchReportsChangesInTheDirectory(t *testing.T) {
	dir := t.TempDir()
	r, w := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- runWatch([]string{dir}, w); _ = w.Close() }()

	events := make(chan watchEvent, 32)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var ev watchEvent
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events <- ev
			}
		}
		close(events)
	}()
	next := func() watchEvent {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the watch ended early")
			}
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no event within 5s")
		}
		return watchEvent{}
	}
	want := func(op, name string) {
		t.Helper()
		for {
			ev := next()
			if ev.Op == op && ev.Name == name {
				return
			}
			if ev.Op == "write" || ev.Op == "chmod" { // a create can bring either along
				continue
			}
			t.Fatalf("got %+v, want op=%s name=%s", ev, op, name)
		}
	}

	want("ready", dir)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want("create", f)
	if err := os.Rename(f, filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	want("rename", f)
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	for ev := next(); ev.Op != "remove"; ev = next() {
		if ev.Op != "rename" {
			t.Fatalf("got %+v while waiting for the remove", ev)
		}
	}

	// Removing the directory itself ends the watch cleanly.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("watch exited %d when its directory went away, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch outlived its directory")
	}
}
