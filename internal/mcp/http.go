package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// wantsToolCall reports whether the message (single or batch) invokes a tool.
// It must mirror HandleMessage's framing exactly, per element, or a batch the
// dispatcher runs skips the 401 that starts the OAuth flow.
func wantsToolCall(body []byte) bool {
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var msgs []json.RawMessage
		if json.Unmarshal(body, &msgs) != nil {
			return false
		}
		for _, m := range msgs {
			if methodOf(m) == "tools/call" {
				return true
			}
		}
		return false
	}
	return methodOf(body) == "tools/call"
}

// methodOf returns the JSON-RPC method name, or "" for anything that is not an object.
func methodOf(raw []byte) string {
	var m struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m.Method
}

// publicOrigin reconstructs the origin the same way the OAuth endpoints do,
// honouring X-Forwarded-Proto from the TLS-terminating proxy.
func publicOrigin(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	return proto + "://" + r.Host
}

// HTTPServer serves stateless MCP over Streamable HTTP. Tool calls dispatch
// in-process to the /v1 router with the caller's Authorization header intact,
// so /v1 keeps enforcing auth, org scoping and limits.
type HTTPServer struct {
	API http.Handler // must route /v1/*
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP endpoint accepts POST only (stateless streamable HTTP)", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	auth := r.Header.Get("Authorization")

	// Discovery is open; tools/call needs a bearer token. The WWW-Authenticate
	// challenge is what makes MCP clients start the browser sign-in flow.
	if auth == "" && wantsToolCall(body) {
		base := publicOrigin(r)
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(replyErr(nil, -32001,
			"authentication required: complete the OAuth flow your client just started, "+
				"or configure the header Authorization: Bearer <api-key> (dashboard → API keys)"))
		return
	}
	be := func(method, path string, reqBody []byte, ctype string) (int, []byte, error) {
		req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		rec := httptest.NewRecorder()
		s.API.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes(), nil
	}
	resp := HandleMessage(body, be)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}
