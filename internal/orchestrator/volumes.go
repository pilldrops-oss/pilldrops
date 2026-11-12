package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// A volume's image lives on the node that created it, so the control plane
// places a new volume like a sandbox, sends its delete to that node, and pins
// a create attaching it there. Reads come from the shared store, so listing
// works while the node is away.
func (s *Server) registerVolumeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/volumes", s.auth(s.handleCreateVolume))
	mux.HandleFunc("GET /v1/volumes", s.auth(s.handleListVolumes))
	mux.HandleFunc("GET /v1/volumes/{id}", s.auth(s.handleGetVolume))
	mux.HandleFunc("DELETE /v1/volumes/{id}", s.auth(s.handleDeleteVolume))
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if !s.reg.AnyWithCapability(cluster.CapVolumes, s.now(), cluster.NodeTTL) {
		writeErr(w, http.StatusNotImplemented, "no online node can hold volumes; they need the firecracker runtime")
		return
	}
	node := s.reg.PickAndReserve(cluster.Request{Volumes: true, MostRoom: true}, s.now(), cluster.NodeTTL)
	if node == nil {
		writeErr(w, http.StatusServiceUnavailable, "no node with room for a volume right now; retry shortly")
		return
	}
	s.relayTo(w, r, node, "POST", "/v1/volumes", body)
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, r.Method, r.URL.Path)
		return
	}
	vols, err := st.ListVolumes(r.Context(), fastOrg(r))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
}

func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, r.Method, r.URL.Path)
		return
	}
	v, ok, err := st.GetVolume(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !ok || v.OrgID != fastOrg(r) {
		writeErr(w, http.StatusNotFound, "volume not found")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	node, err := s.volumeNode(r.Context(), fastOrg(r), []string{r.PathValue("id")})
	if err != nil {
		volumeNodeErr(w, err)
		return
	}
	if node == nil {
		s.proxyToNode(w, r, r.Method, r.URL.Path)
		return
	}
	s.relayTo(w, r, node, "DELETE", r.URL.Path, nil)
}

var (
	errVolumeNotFound = errors.New("volume not found")
	errVolumesSplit   = errors.New("these volumes live on different nodes, and a sandbox runs on one; attach volumes from one node")
	errVolumeNodeAway = errors.New("the node holding the volume is offline or cordoned; it can be used again when that node takes sandboxes")
)

// volumeNode is the one node holding every named volume of org. nil with no
// error means there is no shared store to ask, so the caller lets the single
// node decide.
func (s *Server) volumeNode(ctx context.Context, org string, ids []string) (*cluster.Node, error) {
	st := s.st.Load()
	if st == nil || len(ids) == 0 {
		return nil, nil
	}
	nodeID := ""
	for _, id := range ids {
		v, ok, err := st.GetVolume(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok || v.OrgID != org {
			return nil, fmt.Errorf("%w: %s", errVolumeNotFound, id)
		}
		if nodeID != "" && v.NodeID != nodeID {
			return nil, errVolumesSplit
		}
		nodeID = v.NodeID
	}
	node, ok := s.reg.Node(nodeID)
	if !ok || !node.Schedulable(s.now(), cluster.NodeTTL) {
		return nil, fmt.Errorf("%w: %s", errVolumeNodeAway, strings.Join(ids, ", "))
	}
	return node, nil
}

func volumeNodeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errVolumeNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errVolumesSplit):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	}
}

// relayTo forwards a request to one named node, as the caller's org.
func (s *Server) relayTo(w http.ResponseWriter, r *http.Request, node *cluster.Node, method, path string, body []byte) {
	resp, err := s.forward(r.Context(), node, method, path, body, r.Header.Get("Content-Type"), fastOrg(r))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "node request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort: relayed below
	relay(w, resp)
}
