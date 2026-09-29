package hoststats

import "testing"

func TestParseMeminfo(t *testing.T) {
	in := []byte("MemTotal:       65011712 kB\nMemFree: 1000 kB\nMemAvailable:   43220992 kB\n")
	total, avail := parseMeminfo(in)
	if total != 65011712/1024 || avail != 43220992/1024 {
		t.Fatalf("meminfo = %d,%d MB", total, avail)
	}
}

func TestParseLoadavg(t *testing.T) {
	got := parseLoadavg([]byte("4.10 3.20 2.00 3/1234 5678\n"))
	if got[0] != 4.10 || got[1] != 3.20 || got[2] != 2.00 {
		t.Fatalf("loadavg = %v", got)
	}
}

func TestCPUBusy(t *testing.T) {
	// busy = (900-750)/900 = 16.67%
	prev := cpuTimes{total: 1000, idle: 850}
	cur := cpuTimes{total: 1900, idle: 1600}
	got := cpuBusy(prev, cur)
	if got < 16.6 || got > 16.7 {
		t.Fatalf("cpuBusy = %.2f, want ~16.67", got)
	}
}

func TestParseStat(t *testing.T) {
	got := parseStat([]byte("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4\n"))
	if got.total != 1000 || got.idle != 850 {
		t.Fatalf("parseStat = %+v", got)
	}
}
