package orchestrator

import (
	"errors"
	"testing"
	"time"
)

func TestStaleCacheServesTheLastAnswerOnlyWhenTheStoreFails(t *testing.T) {
	var c staleCache
	now := time.Unix(1000, 0)
	loads := 0
	ok := func(v string) func() (any, error) { return func() (any, error) { loads++; return v, nil } }
	down := func() (any, error) { loads++; return nil, errors.New("store down") }

	if _, err := c.get("k", 0, now, down); err == nil {
		t.Fatal("nothing cached and the store is down: must be an error")
	}
	if v, _ := c.get("k", 0, now, ok("v1")); v != "v1" {
		t.Fatal("first load")
	}
	if v, _ := c.get("k", 0, now, ok("v2")); v != "v2" || loads != 3 {
		t.Fatal("fresh 0 must read the store every time, so a change applies at once")
	}
	if v, err := c.get("k", 0, now.Add(time.Minute), down); err != nil || v != "v2" {
		t.Fatalf("store down: got %v, %v; want the last answer", v, err)
	}
	if _, err := c.get("k", 0, now.Add(previewStaleMax+time.Second), down); err == nil {
		t.Fatal("an answer older than previewStaleMax was served")
	}
}
