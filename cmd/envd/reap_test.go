package main

import (
	"testing"
	"time"
)

func TestRespawnBacksOffAndGivesUp(t *testing.T) {
	var p respawnPolicy
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	for n, w := range want {
		if d, ok := p.next(0); !ok || d != w {
			t.Fatalf("quick exit %d: delay %v ok %v, want %v", n+1, d, ok, w)
		}
	}
	for range respawnLimit - len(want) {
		if _, ok := p.next(0); !ok {
			t.Fatal("gave up before respawnLimit quick exits")
		}
	}
	if _, ok := p.next(0); ok {
		t.Fatal("a server that keeps exiting at once is started forever")
	}

	var healthy respawnPolicy
	for range respawnLimit {
		healthy.next(0)
	}
	if d, ok := healthy.next(healthyRun); !ok || d != respawnMin {
		t.Fatalf("after a healthy run: delay %v ok %v, want %v and a fresh count", d, ok, respawnMin)
	}
}
