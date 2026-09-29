package daemon

import "time"

// Usage is one metering delta or a cumulative total for a sandbox.
type Usage struct {
	VCPUSeconds      float64
	RAMGBSeconds     float64
	StorageGBSeconds float64
	// RunSeconds is wall-clock running time. Not derivable from VCPUSeconds
	// once summed across sandboxes of different sizes.
	RunSeconds float64
}

// IsZero reports whether nothing accrued.
func (u Usage) IsZero() bool {
	return u.VCPUSeconds == 0 && u.RAMGBSeconds == 0 && u.StorageGBSeconds == 0 && u.RunSeconds == 0
}

// meter accrues usage for one sandbox. Compute and storage keep separate
// clocks: compute stops while paused, storage does not, since a paused disk
// still occupies a real one.
type meter struct {
	vcpu      int
	memMiB    int
	storageMB int
	last      time.Time // compute checkpoint; frozen while paused
	stLast    time.Time // storage checkpoint; never frozen
	running   bool
	// ramOnly is standby: the node still holds the sandbox's memory, so
	// RAM-seconds keep accruing while vCPU-seconds stop. A disk pause stops both.
	ramOnly bool

	// acc is the cumulative total checkpointed by sample; in-memory only.
	acc Usage
}

func newMeter(vcpu, memMiB, storageMB int, now time.Time) *meter {
	return &meter{vcpu: vcpu, memMiB: memMiB, storageMB: storageMB, last: now, stLast: now, running: true}
}

func (m *meter) storageGB() float64 { return float64(m.storageMB) / 1024.0 }

// sample returns the usage accrued since the last checkpoint and advances it.
func (m *meter) sample(now time.Time) Usage {
	var d Usage

	stSecs := now.Sub(m.stLast).Seconds()
	if stSecs < 0 {
		stSecs = 0
	}
	m.stLast = now
	d.StorageGBSeconds = m.storageGB() * stSecs

	if m.running {
		secs := now.Sub(m.last).Seconds()
		if secs < 0 {
			secs = 0
		}
		d.RAMGBSeconds = (float64(m.memMiB) / 1024.0) * secs
		if !m.ramOnly {
			d.RunSeconds = secs
			d.VCPUSeconds = float64(m.vcpu) * secs
		}
	}
	m.last = now

	m.acc.VCPUSeconds += d.VCPUSeconds
	m.acc.RAMGBSeconds += d.RAMGBSeconds
	m.acc.StorageGBSeconds += d.StorageGBSeconds
	m.acc.RunSeconds += d.RunSeconds
	return d
}

// peek returns cumulative usage including the current interval, without advancing the checkpoint.
func (m *meter) peek(now time.Time) Usage {
	u := m.acc
	if secs := now.Sub(m.stLast).Seconds(); secs > 0 {
		u.StorageGBSeconds += m.storageGB() * secs
	}
	if m.running {
		if secs := now.Sub(m.last).Seconds(); secs > 0 {
			u.RAMGBSeconds += (float64(m.memMiB) / 1024.0) * secs
			if !m.ramOnly {
				u.RunSeconds += secs
				u.VCPUSeconds += float64(m.vcpu) * secs
			}
		}
	}
	return u
}

// total returns the cumulative usage checkpointed so far; call sample first for a final figure.
func (m *meter) total() Usage { return m.acc }

// freeze stops compute accrual. It does not flush; callers must sample first
// or the pending delta is dropped.
func (m *meter) freeze(now time.Time) {
	m.last = now
	m.running = false
	m.ramOnly = false
}

// standby stops vCPU accrual but keeps RAM-seconds running: the sandbox is
// frozen in memory the node is still holding. Like freeze, it does not flush.
func (m *meter) standby(now time.Time) {
	m.last = now
	m.running = true
	m.ramOnly = true
}

// resume restarts full compute accrual from now.
func (m *meter) resume(now time.Time) {
	m.last = now
	m.running = true
	m.ramOnly = false
}
