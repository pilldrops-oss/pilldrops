//go:build !core

package httpapi

import (
	"errors"
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/extras/functions"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// autoscaleDefaultReplicas mirrors the registry's unexported default so the
// plan clamps the number actually used, not just the one the caller typed.
const autoscaleDefaultReplicas = 3

// clampReplicas bounds a function's burst ceiling by the org's concurrency cap.
func clampReplicas(requested, maxConcurrent int) int {
	if requested <= 0 {
		requested = autoscaleDefaultReplicas
	}
	if maxConcurrent <= 0 {
		return requested
	}
	return min(max(requested, 1), maxConcurrent)
}

func (s *Server) handleFuncRegister(w http.ResponseWriter, r *http.Request) {
	if s.functions == nil {
		writeErr(w, http.StatusNotFound, "functions not enabled")
		return
	}
	var spec functions.RegisterSpec
	if !decodeBody(r, &spec) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if spec.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	if spec.Template == "" {
		spec.Template = s.templates[0]
	}
	// Store the canonical template ID, not an alias; the VM layer looks up the rootfs by it.
	tmpl, ok := templates.Resolve(spec.Template)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown template: "+spec.Template)
		return
	}
	spec.Template = tmpl.ID
	plan := s.orgPlan(r.Context(), orgID(r))
	vcpu := spec.VCPU
	if vcpu <= 0 {
		vcpu = tmpl.CPU
	}
	memMB := spec.MemoryMB
	if memMB <= 0 {
		memMB = tmpl.MemoryMB
	}
	spec.VCPU = min(max(vcpu, 1), plan.MaxVCPU)
	spec.MemoryMB = min(max(memMB, 128), plan.MaxMemMB)
	spec.MaxReplicas = clampReplicas(spec.MaxReplicas, plan.MaxConcurrent)
	fn, err := s.functions.Register(r.Context(), orgID(r), spec)
	if errors.Is(err, functions.ErrExists) {
		writeErr(w, http.StatusConflict, "function already exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, fn)
}

func (s *Server) handleFuncList(w http.ResponseWriter, r *http.Request) {
	if s.functions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"functions": []any{}})
		return
	}
	list, err := s.functions.List(r.Context(), orgID(r))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	for i := range list {
		list[i].Replicas = s.functions.ReplicaCount(orgID(r), list[i].Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"functions": list})
}

func (s *Server) handleFuncGet(w http.ResponseWriter, r *http.Request) {
	fn, ok := s.lookupFunc(w, r)
	if !ok {
		return
	}
	fn.Replicas = s.functions.ReplicaCount(orgID(r), fn.Name)
	writeJSON(w, http.StatusOK, fn)
}

func (s *Server) handleFuncUpdate(w http.ResponseWriter, r *http.Request) {
	if s.functions == nil {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	var spec functions.RegisterSpec
	if !decodeBody(r, &spec) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tmpl, ok := templates.Resolve(spec.Template)
	if spec.Template != "" && !ok {
		writeErr(w, http.StatusBadRequest, "unknown template: "+spec.Template)
		return
	}
	plan := s.orgPlan(r.Context(), orgID(r))
	vcpu := spec.VCPU
	if vcpu <= 0 {
		vcpu = tmpl.CPU
	}
	memMB := spec.MemoryMB
	if memMB <= 0 {
		memMB = tmpl.MemoryMB
	}
	spec.VCPU = min(max(vcpu, 1), plan.MaxVCPU)
	spec.MemoryMB = min(max(memMB, 128), plan.MaxMemMB)
	spec.MaxReplicas = clampReplicas(spec.MaxReplicas, plan.MaxConcurrent)
	err := s.functions.Update(r.Context(), orgID(r), r.PathValue("name"), spec)
	if errors.Is(err, functions.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleFuncDelete(w http.ResponseWriter, r *http.Request) {
	if s.functions == nil {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	err := s.functions.Delete(r.Context(), orgID(r), r.PathValue("name"))
	if errors.Is(err, functions.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleFuncCode(w http.ResponseWriter, r *http.Request) {
	if s.functions == nil {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	files, err := s.functions.Code(r.Context(), orgID(r), r.PathValue("name"))
	if errors.Is(err, functions.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// lookupFunc resolves {name} for the caller's org; writes a 404 and returns false if absent.
func (s *Server) lookupFunc(w http.ResponseWriter, r *http.Request) (functions.Function, bool) {
	if s.functions == nil {
		writeErr(w, http.StatusNotFound, "function not found")
		return functions.Function{}, false
	}
	fn, ok, err := s.functions.Get(r.Context(), orgID(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return functions.Function{}, false
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "function not found")
		return functions.Function{}, false
	}
	return fn, true
}
