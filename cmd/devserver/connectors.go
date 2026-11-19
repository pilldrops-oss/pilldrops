package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// connectorProxy forwards the connector routes that authenticate themselves, a
// tunnel token or a pd_conn_ key, with that credential and any WebSocket
// upgrade untouched. X-Forwarded-Host and -Proto tell the orchestrator the
// public origin to put in a session's tunnel URLs. nil if orchURL is unparseable.
func connectorProxy(orchURL string) http.Handler {
	base, err := url.Parse(strings.TrimRight(orchURL, "/"))
	if err != nil || base.Host == "" {
		return nil
	}
	rp := httputil.NewSingleHostReverseProxy(base)
	rp.FlushInterval = -1
	director := rp.Director
	rp.Director = func(req *http.Request) {
		proto := req.Header.Get("X-Forwarded-Proto")
		if proto == "" {
			proto = "http"
			if req.TLS != nil {
				proto = "https"
			}
		}
		director(req)
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set("X-Forwarded-Proto", proto)
		req.Host = base.Host
		req.Header.Del("Cookie")
	}
	return rp
}

// registerConnectorRoutes mounts the connector routes outside the portal's
// auth. The tunnel and data sockets and a pd_conn_ session go straight to the
// orchestrator; a session asked for with any other credential goes through
// authed, as every portal API call does. /internal/private/dial is not here:
// nodes reach the orchestrator directly.
func registerConnectorRoutes(r chi.Router, sockets, authed http.Handler) {
	r.Get("/v1/connectors/{id}/tunnel", sockets.ServeHTTP)
	r.Get("/v1/connector-conns/{conn}", sockets.ServeHTTP)
	r.Post("/v1/connectors/session", func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(presentedToken(req), "pd_conn_") {
			sockets.ServeHTTP(w, req)
			return
		}
		authed.ServeHTTP(w, req)
	})
}
