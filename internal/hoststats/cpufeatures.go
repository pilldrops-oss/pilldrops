package hoststats

import (
	"os"
	"sort"
	"strings"
	"sync"
)

// The host's instruction-set identity, for placing and restoring memory
// snapshots.
//
// A Firecracker memory snapshot is a frozen process image, and the processes
// inside it have already chosen their code paths from CPUID. Restore one on a
// CPU poorer than the one that captured it and the guest dies on the first
// instruction the host does not implement. Measured on the two-node fleet: a
// snapshot taken on a Haswell node restored on an Ivy Bridge node loaded
// cleanly and the guest's Go runtime then executed VPBROADCASTB (AVX2), giving
// SIGILL with instruction bytes c4 e2 7d 78. The direction is the whole point:
// poorer-to-richer is fine, richer-to-poorer is fatal.

// dispatchFlags are the /proc/cpuinfo flags a compiler, libc or language
// runtime actually selects code paths on -- the x86-64-v2/v3/v4 sets plus the
// crypto and bit-manipulation extensions ifunc resolvers dispatch over.
//
// An allowlist rather than every flag: two hosts of the same generation differ
// on dozens of flags that gate nothing a guest can execute (mitigation bits,
// power management, topology), and comparing the whole set would refuse
// restores that work. Anything left out can only cause a restore this cannot
// predict, which is why the list errs towards including a flag.
var dispatchFlags = []string{
	// x86-64-v2
	"cx16", "lahf_lm", "popcnt", "sse4_1", "sse4_2", "ssse3",
	// x86-64-v3 -- the level the fleet failure was in
	"avx", "avx2", "bmi1", "bmi2", "f16c", "fma", "abm", "movbe", "xsave",
	// x86-64-v4
	"avx512f", "avx512bw", "avx512cd", "avx512dq", "avx512vl",
	// Dispatched on by libc and language runtimes below any level
	"aes", "pclmulqdq", "rdrand", "rdseed", "adx", "sha_ni", "erms", "fsrm",
}

var cpuFeaturesOnce struct {
	sync.Once
	list []string
}

// CPUFeatures is the dispatch-relevant instruction set this host provides,
// sorted. Empty when it cannot be determined -- a non-Linux host, or an
// architecture whose /proc/cpuinfo does not spell flags this way. Empty means
// "unknown", never "none": a caller must treat it as no answer rather than as
// a CPU with no features.
//
// Read once. CPUID does not change under a running kernel, and this is on the
// heartbeat and the capture path.
func CPUFeatures() []string {
	cpuFeaturesOnce.Do(func() {
		b, err := os.ReadFile("/proc/cpuinfo")
		if err != nil {
			return
		}
		cpuFeaturesOnce.list = parseCPUFlags(b)
	})
	return cpuFeaturesOnce.list
}

// parseCPUFlags picks the dispatch-relevant flags out of /proc/cpuinfo. Only
// the first core's line is read: a heterogeneous-core host would need a guest
// pinned to a core class to matter, and Firecracker gives every vCPU the same
// CPUID.
func parseCPUFlags(b []byte) []string {
	want := make(map[string]bool, len(dispatchFlags))
	for _, f := range dispatchFlags {
		want[f] = true
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "flags" {
			continue
		}
		var out []string
		for _, f := range strings.Fields(val) {
			if want[f] {
				out = append(out, f)
			}
		}
		sort.Strings(out)
		return out
	}
	return nil
}

// MissingCPUFeatures returns the entries of want that are not in have,
// sorted. Empty means have is a superset: everything the capture chose its code
// paths from is present, so a restore is safe in that direction.
//
// An empty want is always satisfied -- a capture that recorded nothing predates
// the record, and refusing every one of those would strand snapshots that
// restore correctly. An empty have with a non-empty want is NOT satisfied: "the
// host could not be read" and "the host has everything" must not look the same.
func MissingCPUFeatures(want, have []string) []string {
	if len(want) == 0 {
		return nil
	}
	present := make(map[string]bool, len(have))
	for _, f := range have {
		present[f] = true
	}
	var missing []string
	for _, f := range want {
		if !present[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	return missing
}
