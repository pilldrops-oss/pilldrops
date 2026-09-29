//go:build linux

package uffd

import "testing"

func TestCheckRegions(t *testing.T) {
	const mib = 1 << 20
	ok := []region{{Base: 0x7f0000000000, Size: 256 * mib}, {Base: 0x7f0010000000, Size: 256 * mib, Offset: 256 * mib}}
	if err := checkRegions(ok, 512*mib); err != nil {
		t.Fatalf("a layout the size of the VM was refused: %v", err)
	}
	for name, rs := range map[string][]region{
		"none":             nil,
		"empty region":     {{Base: 0x1000}},
		"base wraps":       {{Base: ^uint64(0) - mib, Size: 2 * mib}},
		"offset wraps":     {{Base: 0x1000, Size: 2 * mib, Offset: ^uint64(0)}},
		"more than the VM": ok[:1:1],
		"total wraps":      {{Base: 0x1000, Size: 1 << 63}, {Base: 0x1000, Size: 1 << 63}},
	} {
		max := uint64(512 * mib)
		if name == "more than the VM" {
			max = 128 * mib
		}
		if err := checkRegions(rs, max); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
