package httpapi

// The node end of a port forward. The control plane's ingress terminates the
// public port and dials this route over the existing pinned node hop; the node
// hijacks the connection and splices raw bytes to the guest. Nothing here
// interprets the payload, so any TCP protocol works.

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// forwardDialTimeout bounds the dial to the guest after the wake.
const forwardDialTimeout = 10 * time.Second

// registerForwardRoutes mounts the raw L4 tunnels. Not behind s.auth: the
// credential is the sandbox+port-scoped preview token, the same one the preview
// proxy carries, so a compromised ingress cannot reach another sandbox. raw()
// rather than route() because this is a customer's own bytes, not an API call.
func (s *Server) registerForwardRoutes(raw func(string, http.Handler)) {
	raw("GET /v1/sandboxes/{id}/tcp/{port}", s.throttleByIP(http.HandlerFunc(s.handleForwardTCP)))
	raw("GET /v1/sandboxes/{id}/udp/{port}", s.throttleByIP(http.HandlerFunc(s.handleForwardUDP)))
}

// forwardTarget runs the shared front half of both tunnels: the token gate, the
// wake, and resolving the guest address. It writes the refusal and returns ""
// when the caller must stop. Every refusal is the same 404 an unknown sandbox
// gets, so the route cannot be used to enumerate ids or ports.
func (s *Server) forwardTarget(w http.ResponseWriter, r *http.Request) (target string, port int, ok bool) {
	id := r.PathValue("id")
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return "", 0, false
	}
	verdict, org := s.checkPreviewAuth(id, port, r.URL.Query().Get("ptok"))
	if verdict != previewAuthOK || org == "" {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return "", 0, false
	}
	// PreviewTarget is the wake: a paused or standby sandbox is resumed and
	// Touched before the address comes back, so a forward behaves like a
	// preview request rather than failing on an idle sandbox.
	target, err = s.d.PreviewTarget(r.Context(), id, port, org)
	if err != nil || target == "" {
		slog.WarnContext(r.Context(), "port forward could not resolve a guest target",
			"component", "forward", "sandbox", id, "port", port, "err", err)
		writeErr(w, statusForErr(err), "no app on port "+strconv.Itoa(port))
		return "", 0, false
	}
	return target, port, true
}

func (s *Server) handleForwardTCP(w http.ResponseWriter, r *http.Request) {
	target, port, ok := s.forwardTarget(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	guest, err := (&net.Dialer{Timeout: forwardDialTimeout}).DialContext(r.Context(), "tcp", target)
	if err != nil {
		// Dial first, hijack second: before the hijack an error is still an
		// HTTP response the ingress can log.
		writeErr(w, http.StatusBadGateway, "guest port "+strconv.Itoa(port)+" refused the connection")
		return
	}

	client, err := hijackRaw(w)
	if err != nil {
		_ = guest.Close() //nolint:errcheck // best-effort
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Connection: Upgrade\r\nUpgrade: pilldrops-l4\r\n\r\n"); err != nil {
		_ = client.Close() //nolint:errcheck // best-effort
		_ = guest.Close()  //nolint:errcheck // best-effort
		return
	}
	slog.InfoContext(r.Context(), "port forward open",
		"component", "forward", "sandbox", id, "port", port, "target", target)
	httpx.Splice(client, guest)
}

// hijackRaw takes the raw connection off the ResponseWriter, returning any
// bytes the server already buffered along with it.
func hijackRaw(w http.ResponseWriter) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("this server cannot hand back a raw connection")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if rw != nil && rw.Reader.Buffered() > 0 {
		// A bufio.Reader over conn keeps reading from conn once its buffer
		// drains, so this loses nothing the client already sent.
		return httpx.Prefixed(conn, rw.Reader), nil
	}
	return conn, nil
}
