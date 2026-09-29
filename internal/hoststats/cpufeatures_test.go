package hoststats

import (
	"slices"
	"strings"
	"testing"
)

// The two CPUs the SIGILL was measured between. Trimmed to the flags that
// matter, in /proc/cpuinfo's own shape.
const (
	ivyBridge = "processor\t: 0\nmodel name\t: Intel(R) Xeon(R) CPU E3-1240 V2\n" +
		"flags\t\t: fpu vme de pse tsc msr pae mce cx16 sse4_1 sse4_2 ssse3 popcnt " +
		"aes xsave avx f16c rdrand lahf_lm erms pti ssbd\n"
	haswell = "processor\t: 0\nmodel name\t: Intel(R) Xeon(R) CPU E3-1231 v3\n" +
		"flags\t\t: fpu vme de pse tsc msr pae mce cx16 sse4_1 sse4_2 ssse3 popcnt " +
		"aes xsave avx f16c rdrand lahf_lm erms pti ssbd avx2 bmi1 bmi2 fma movbe abm " +
		"rdseed adx\n"
)

// TestRicherToPoorerIsRefusedAndPoorerToRicherIsNot is the check for the
// snapshot that killed its guest. Direction is the entire point: a Haswell
// capture restored on Ivy Bridge took SIGILL on an AVX2 VPBROADCASTB, while the
// reverse restores fine.
//
// It fails against the old behaviour trivially, because the old behaviour
// recorded nothing and compared nothing.
func TestRicherToPoorerIsRefusedAndPoorerToRicherIsNot(t *testing.T) {
	poor, rich := parseCPUFlags([]byte(ivyBridge)), parseCPUFlags([]byte(haswell))
	if len(poor) == 0 || len(rich) == 0 {
		t.Fatal("neither CPU parsed; the rest of this test would pass on two empty sets")
	}
	if slices.Contains(poor, "avx2") || !slices.Contains(rich, "avx2") {
		t.Fatalf("the fixtures do not differ on avx2 (poor=%v rich=%v), so this proves nothing",
			poor, rich)
	}

	missing := MissingCPUFeatures(rich, poor)
	if len(missing) == 0 {
		t.Fatal("restoring a Haswell capture on Ivy Bridge was allowed — that is the SIGILL")
	}
	for _, want := range []string{"avx2", "bmi1", "bmi2", "fma"} {
		if !slices.Contains(missing, want) {
			t.Errorf("the refusal does not name %s; an operator cannot act on it. got %v", want, missing)
		}
	}
	if got := MissingCPUFeatures(poor, rich); len(got) != 0 {
		t.Errorf("restoring an Ivy Bridge capture on Haswell was refused, missing %v — "+
			"poorer-to-richer is safe and refusing it strands working snapshots", got)
	}
}

// TestAnUnknownHostIsNotASilentPass is the trap this whole defect came out of:
// nothing was recorded anywhere, so every comparison was between two empty
// values and every restore looked compatible. A capture that names features
// must not be waved through by a host that could not be read.
func TestAnUnknownHostIsNotASilentPass(t *testing.T) {
	if got := MissingCPUFeatures([]string{"avx2"}, nil); len(got) == 0 {
		t.Fatal("a capture needing avx2 was satisfied by a host reporting nothing — " +
			"'the host has everything' and 'the host could not be read' must not look the same")
	}
	// The one direction that is a pass: a capture that recorded nothing predates
	// the record, and refusing those strands snapshots that restore correctly.
	if got := MissingCPUFeatures(nil, parseCPUFlags([]byte(ivyBridge))); len(got) != 0 {
		t.Fatalf("a pre-record capture was refused, missing %v", got)
	}
}

// TestOnlyDispatchFlagsAreCompared keeps the allowlist doing its job: comparing
// whole flag lines would refuse restores between two hosts that differ only on
// mitigation and topology bits.
func TestOnlyDispatchFlagsAreCompared(t *testing.T) {
	got := parseCPUFlags([]byte(haswell))
	for _, noise := range []string{"fpu", "vme", "pti", "ssbd", "msr"} {
		if slices.Contains(got, noise) {
			t.Errorf("%q is compared but gates nothing a guest executes: %s", noise, strings.Join(got, ","))
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("features are not sorted, so two readings of one host can differ: %v", got)
	}
}
