package httpapi

import (
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/hoststats"
)

// handleHost returns a host resource snapshot plus the CPU/RAM committed to live sandboxes.
func (s *Server) handleHost(w http.ResponseWriter, _ *http.Request) {
	st := hoststats.Sample(s.dataDir)
	boxes := s.d.List("")
	vcpu, memMB := 0, 0
	for _, b := range boxes {
		vcpu += b.VCPU
		memMB += b.MemMiB
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cpu_cores": st.Cores, "cpu_percent": st.CPUPercent, "load_avg": st.LoadAvg,
		"mem_total_mb": st.MemTotalMB, "mem_used_mb": st.MemUsedMB, "mem_available_mb": st.MemAvailMB,
		"disk_total_gb": st.DiskTotalGB, "disk_used_gb": st.DiskUsedGB, "disk_free_gb": st.DiskFreeGB,
		"sandboxes_active": len(boxes), "vcpu_committed": vcpu, "mem_committed_mb": memMB,
	})
}
