package docker

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// dockerdPIDFile is dockerd's default --pidfile.
var dockerdPIDFile = "/var/run/docker.pid"

// dockerdStarted is when the local dockerd started, or zero when unknown (a
// remote daemon, a non-default pidfile).
func dockerdStarted() time.Time {
	b, err := os.ReadFile(dockerdPIDFile)
	if err != nil {
		return time.Time{}
	}
	return procStarted(strings.TrimSpace(string(b)))
}

// procStarted reads a process's start time from /proc: its starttime in clock
// ticks since boot (USER_HZ, 100 on every Linux this runs on) plus btime.
func procStarted(pid string) time.Time {
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return time.Time{}
	}
	// Fields after the comm's closing paren start at field 3; starttime is 22.
	f := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(f) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	sys, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(sys), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * 10 * time.Millisecond)
		}
	}
	return time.Time{}
}
