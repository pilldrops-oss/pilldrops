package cluster

import (
	"testing"
	"time"
)

func TestMemJoinStoreTokenIsOneTime(t *testing.T) {
	js := NewMemJoinStore()
	now := time.Unix(1_700_000_000, 0)

	tok := js.Mint(now, 15*time.Minute)
	if len(tok) < 20 || tok[:4] != "sfj_" {
		t.Fatalf("token = %q, want sfj_-prefixed random", tok)
	}
	if !js.Consume(tok, now.Add(time.Minute)) {
		t.Fatal("first consume should succeed")
	}
	if js.Consume(tok, now.Add(time.Minute)) {
		t.Fatal("second consume must fail (one-time)")
	}
}

func TestMemJoinStoreTokenExpires(t *testing.T) {
	js := NewMemJoinStore()
	now := time.Unix(1_700_000_000, 0)
	tok := js.Mint(now, 15*time.Minute)
	if js.Consume(tok, now.Add(16*time.Minute)) {
		t.Fatal("expired token must not consume")
	}
	if js.Consume("sfj_never-minted", now) {
		t.Fatal("unknown token must not consume")
	}
}

func TestMemJoinStoreAllocatesUniqueNames(t *testing.T) {
	js := NewMemJoinStore()

	a := js.AllocateName("", []string{})
	b := js.AllocateName("", []string{a})
	if a == b {
		t.Fatalf("auto names collided: %q == %q", a, b)
	}

	if got := js.AllocateName("gpu-box", []string{"node-1"}); got != "gpu-box" {
		t.Fatalf("free suggestion = %q, want gpu-box", got)
	}
	got := js.AllocateName("gpu-box", []string{"gpu-box"})
	if got == "gpu-box" {
		t.Fatalf("taken suggestion returned duplicate %q", got)
	}
}
