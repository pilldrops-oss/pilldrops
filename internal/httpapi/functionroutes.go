//go:build !core

package httpapi

import (
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/extras/functions"
)

// registerFunctionRoutes mounts the serverless-function routes. The invoke
// patterns go through raw, not route, so a user's handler does not inherit
// the platform's request-body cap.
func (s *Server) registerFunctionRoutes(route, raw func(string, http.Handler)) {
	route("POST /v1/functions", s.auth(s.handleFuncRegister))
	route("GET /v1/functions", s.auth(s.handleFuncList))
	route("GET /v1/functions/{name}", s.auth(s.handleFuncGet))
	route("PUT /v1/functions/{name}", s.auth(s.handleFuncUpdate))
	route("DELETE /v1/functions/{name}", s.auth(s.handleFuncDelete))
	route("GET /v1/functions/{name}/logs", s.auth(s.handleFuncLogs))
	route("GET /v1/functions/{name}/code", s.auth(s.handleFuncCode))
	route("GET /v1/functions/{name}/metrics", s.auth(s.handleFuncMetrics))
	raw("/v1/functions/{name}/invoke", s.auth(s.handleFuncInvoke))
	raw("/v1/functions/{name}/invoke/{path...}", s.auth(s.handleFuncInvoke))
}

// functionFeature is the Server state only the function surface uses; the core
// build embeds an empty sibling.
type functionFeature struct {
	functions *functions.Registry
}

// SetFunctions attaches the functions registry. Unset, the function endpoints 404.
func (s *Server) SetFunctions(r *functions.Registry) { s.functions = r }
