package orchestrator

// Store-backed fast paths: list, usage and history are pure reads of the shared
// store, answered here rather than proxied to a node that would query the same
// store back. Each falls back to the node proxy when no store is wired.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// fastOrg is the org a request acts on. A customer key names its own org and
// X-Org-Override is ignored; only an operator may name another org via the header.
func fastOrg(r *http.Request) string {
	if org := authedOrg(r); org != "" {
		return org
	}
	if isOperator(r) {
		if ov := r.Header.Get(orgOverride); ov != "" {
			return ov
		}
	}
	return "default"
}

// actingForOrg is the customer org an operator request names, "" when it names
// none (the operator's own traffic) or the caller is not an operator.
func actingForOrg(r *http.Request) string {
	if !isOperator(r) {
		return ""
	}
	return r.Header.Get(orgOverride)
}

// storeSandboxJSON mirrors daemon.SandboxInfo's wire shape.
type storeSandboxJSON struct {
	ID           string    `json:"id"`
	Template     string    `json:"template"`
	Name         string    `json:"name,omitempty"`
	Slug         string    `json:"slug,omitempty"`
	OrgID        string    `json:"org_id"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	CreatedBy    string    `json:"created_by"`
	Visibility   string    `json:"visibility"`
	PublicPort   int       `json:"public_port,omitempty"`
	VCPU         int       `json:"vcpu,omitempty"`
	MemMiB       int       `json:"mem_mib,omitempty"`
	StorageMB    int       `json:"storage_mb,omitempty"`
	// Node is for the operator's cluster view; tenants never see node ids,
	// the same line /v1/nodes draws.
	Node string `json:"node,omitempty"`
}

// listFromStore serves GET /v1/sandboxes from the store; false means nothing was written.
func (s *Server) listFromStore(w http.ResponseWriter, r *http.Request) bool {
	st := s.st.Load()
	if st == nil {
		return false
	}
	rows, err := st.SandboxesByOrg(r.Context(), fastOrg(r))
	if err != nil {
		return false
	}
	// The same rule the node applies, applied here too: this fast path answers
	// the dashboard's list without a node ever seeing the request, so a private
	// sandbox left in it would be listed to the whole org.
	// Only a key whose own role is operator sees everything, the same rule the
	// node applies: the fleet credential on the wire is the control plane's,
	// not a member's, so it does not by itself open other members' machines.
	keyID, keyRole := callerKey(r.Context())
	operator := keyRole == "operator"
	fleetView := operator || (isOperator(r) && keyID == "")
	out := make([]storeSandboxJSON, 0, len(rows))
	for _, rec := range rows {
		if !sandbox.Visible(rec.ID, rec.Visibility, rec.CreatedBy, keyID, operator) {
			continue
		}
		out = append(out, storeSandboxJSON{
			// Slug: the name every other verb accepts in place of the id, and
			// the list is where a caller goes looking for it.
			ID: rec.ID, Template: rec.Template, Name: rec.Name, Slug: rec.Slug, OrgID: rec.OrgID, State: rec.State,
			CreatedBy: rec.CreatedBy, Visibility: rec.Visibility,
			CreatedAt:    time.Unix(rec.CreatedAt, 0).UTC(),
			ExpiresAt:    time.Unix(rec.ExpiresAt, 0).UTC(),
			LastActiveAt: time.Unix(rec.UpdatedAt, 0).UTC(),
			PublicPort:   rec.PublicPort, VCPU: rec.VCPU, MemMiB: rec.MemMiB, StorageMB: rec.StorageMB,
		})
		if fleetView {
			out[len(out)-1].Node = rec.NodeID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
	return true
}

func round4(f float64) float64 { return float64(int(f*10000)) / 10000 }

// previewOrgOf resolves a sandbox's owner org from the live table, then history.
// ("", false) is safe: nodePreviewToken still mints a secret-bound token without the org binding.
func (s *Server) previewOrgOf(ctx context.Context, id string) (string, bool) {
	st := s.st.Load()
	if st == nil {
		return "", false
	}
	if row, ok, err := s.previewRow(ctx, id); err == nil && ok {
		return row.OrgID, true
	}
	if rec, ok, err := st.GetHistory(ctx, id); err == nil && ok {
		return rec.OrgID, true
	}
	return "", false
}

// usageFromStore serves GET /v1/usage from the store in the node's response shape.
func (s *Server) usageFromStore(w http.ResponseWriter, r *http.Request) bool {
	st := s.st.Load()
	if st == nil {
		return false
	}
	org := fastOrg(r)
	var (
		sum      store.UsageSummary
		sumErr   error
		pts      []store.UsagePoint
		active   int
		byClient map[string]int
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); sum, sumErr = st.UsageSummary(r.Context(), org) }()
	go func() { defer wg.Done(); pts, active = st.UsageSeries(r.Context(), org) }()
	go func() { defer wg.Done(); byClient, _ = st.CreationsByClient(r.Context(), org) }()
	wg.Wait()
	if sumErr != nil {
		return false
	}
	if byClient == nil {
		byClient = map[string]int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_starts": sum.Sandboxes,
		"active":       active,
		"vcpu_hours":   round4(sum.VCPUSeconds / 3600),
		"ram_gb_hours": round4(sum.RAMGBSeconds / 3600),
		"points":       pts,
		"by_client":    byClient,
	})
	return true
}

// historyFromStore serves GET /v1/history from the store in the node's response shape.
func (s *Server) historyFromStore(w http.ResponseWriter, r *http.Request) bool {
	st := s.st.Load()
	if st == nil {
		return false
	}
	limit := httpx.QueryInt(r, "limit", 200)
	offset := httpx.QueryInt(r, "offset", 0)
	recs, err := st.ListHistoryPage(r.Context(), fastOrg(r), limit, offset)
	if err != nil {
		return false
	}
	if recs == nil {
		recs = []store.HistoryRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": recs})
	return true
}
