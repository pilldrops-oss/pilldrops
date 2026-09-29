// Package proxy reverse-proxies a sandbox's web app under /preview/{id}/{port}/
// and <port>-<id>.<domain>, waking paused sandboxes on the first request.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Resolver returns the host:port for a sandbox's app port, waking it if paused.
// org is the authorized org; the resolver refuses other owners and an empty org.
type Resolver interface {
	PreviewTarget(ctx context.Context, id string, port int, org string) (string, error)
}

// TerminalAuthorizer is an optional Resolver capability supplying the
// credential envd's web terminal requires.
type TerminalAuthorizer interface {
	TerminalToken(ctx context.Context, id string) (string, error)
}

// Toucher is an optional Resolver capability: Touch resets the sandbox's idle
// timer. A hijacked websocket (noVNC, the web terminal) calls it every
// touchInterval while the connection lives, so an open viewer counts as activity.
type Toucher interface {
	Touch(id string)
}

var (
	transport     = httpx.NewFanoutTransport()
	touchInterval = 30 * time.Second
)

// keepAwake wraps w so that, if the request is upgraded and hijacked, a ticker
// touches id until the hijacked connection closes. Plain requests pass through.
func keepAwake(w http.ResponseWriter, req *http.Request, r Resolver, id string) http.ResponseWriter {
	t, ok := r.(Toucher)
	if !ok || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return w
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return w
	}
	return &touchingWriter{ResponseWriter: w, hj: hj, t: t, id: id}
}

type touchingWriter struct {
	http.ResponseWriter
	hj http.Hijacker
	t  Toucher
	id string
}

func (tw *touchingWriter) Unwrap() http.ResponseWriter { return tw.ResponseWriter }

func (tw *touchingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := tw.hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(touchInterval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				tw.t.Touch(tw.id)
			}
		}
	}()
	return &touchConn{Conn: conn, stop: func() { close(done) }}, rw, nil
}

// touchConn stops the ticker when ReverseProxy closes the hijacked connection.
type touchConn struct {
	net.Conn
	once sync.Once
	stop func()
}

func (c *touchConn) Close() error {
	c.once.Do(c.stop)
	return c.Conn.Close()
}

// Handler returns an http.Handler serving /preview/{id}/{port}/rest.
func Handler(r Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, port, rest, ok := ParsePreviewPath(req.URL.Path)
		if !ok {
			http.Error(w, "bad preview path — expected /preview/{id}/{port}/", http.StatusBadRequest)
			return
		}

		// An empty org means no preview gate ran. Refuse before PreviewTarget,
		// which would wake a paused sandbox on the way to a 404.
		org := previewOrgFrom(req.Context())
		if org == "" {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		target, err := r.PreviewTarget(req.Context(), id, port, org)
		if err != nil {
			// A foreign org is a 404, indistinguishable from a missing sandbox.
			// Transient wake failures are 503 so clients retry.
			switch {
			case errors.Is(err, rt.ErrNotFound), errors.Is(err, rt.ErrForeignOrg):
				http.Error(w, "sandbox not found", http.StatusNotFound)
			case errors.Is(err, rt.ErrNoAppOnPort):
				http.Error(w, err.Error(), http.StatusNotFound)
			default:
				http.Error(w, "sandbox unavailable: "+err.Error(), http.StatusServiceUnavailable)
			}
			return
		}

		// envd gates the terminal's /ws upgrade on this token. Sent as a header
		// so the credential stays out of URLs, access logs and Referer.
		var extra http.Header
		if port == rt.TerminalPort {
			extra = http.Header{}
			if a, ok := r.(TerminalAuthorizer); ok {
				if tok, terr := a.TerminalToken(req.Context(), id); terr == nil && tok != "" {
					// Set, not a map write: MetadataKey is lowercase and needs canonicalising.
					extra.Set(sbtoken.MetadataKey, tok)
				}
			}
			// envd's CheckOrigin requires Origin to match Host, which never holds
			// through a proxy; the request already cleared the preview gate.
			extra.Set("Origin", "http://"+target)
		}

		errBody := fmt.Sprintf(
			"App is not responding on port %d.\n\n"+
				"sandboxd assigns each sandbox a port and injects it as $PORT.\n"+
				"Make your app bind to $PORT, e.g.:\n"+
				"  python: HTTPServer(('0.0.0.0', int(os.environ['PORT'])), ...)\n"+
				"  node:   app.listen(process.env.PORT)\n",
			port)
		ForwardWithHeader(keepAwake(w, req, r, id), req, target, rest, errBody, extra)
	}
}

// Forward reverse-proxies req to target with the path rewritten to targetPath.
// errBody is written with a 502 if the upstream cannot be reached.
func Forward(w http.ResponseWriter, req *http.Request, target, targetPath, errBody string) {
	ForwardWithHeader(w, req, target, targetPath, errBody, nil)
}

// ForwardWithHeader is Forward plus headers set on the outbound request only,
// so node-held credentials never appear on req.
func ForwardWithHeader(w http.ResponseWriter, req *http.Request, target, targetPath, errBody string, extra http.Header) {
	origHost, origProto := req.Host, forwardedProto(req)
	upstreamQuery := withoutPreviewCredentials(req.URL.RawQuery)
	rp := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(pr *http.Request) {
			pr.URL.Scheme = "http"
			pr.URL.Host = target
			pr.URL.Path = targetPath
			pr.URL.RawPath = ""
			pr.URL.RawQuery = upstreamQuery
			pr.Host = target
			pr.Header.Set("X-Forwarded-Host", origHost)
			pr.Header.Set("X-Forwarded-Proto", origProto)
			for k, vs := range extra {
				pr.Header[k] = vs
			}
		},
		ModifyResponse: func(res *http.Response) error {
			setNoReferrer(res.Header)
			return nil
		},
		ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, _ error) {
			setNoReferrer(ew.Header())
			ew.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(ew, errBody)
		},
	}
	rp.ServeHTTP(w, req)
}

// withoutPreviewCredentials drops the preview tokens from the query before the
// request reaches the customer's application.
//
// Both were being forwarded with their values, so every preview request wrote a
// live credential into the tenant's own access log — and into anything the app
// echoes, logs or reports. The gate that reads them has already run by the time
// this is called; nothing upstream has any use for them. The single chokepoint:
// the path form and the <port>-<id> host form both forward through here.
func withoutPreviewCredentials(rawQuery string) string {
	if rawQuery == "" || !strings.Contains(rawQuery, "tok") {
		return rawQuery
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		// Unparseable, so it cannot be edited safely and may still hold a
		// token: the app gets no query rather than a credential.
		return ""
	}
	if !q.Has("token") && !q.Has("ptok") {
		return rawQuery
	}
	q.Del("token")
	q.Del("ptok")
	return q.Encode()
}

// setNoReferrer keeps the ?ptok= credential in a preview URL out of Referer.
// Set on the upstream response, not the ResponseWriter, because ReverseProxy
// appends headers and a tenant app's own Referrer-Policy would otherwise win.
func setNoReferrer(h http.Header) {
	h.Set("Referrer-Policy", "no-referrer")
}

func forwardedProto(req *http.Request) string {
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

// ParsePreviewPath splits /preview/{id}/{port}/rest into its parts. rest always
// begins with "/". Also used by the preview-auth middleware that runs ahead of
// Handler, and by the control plane.
func ParsePreviewPath(p string) (id string, port int, rest string, ok bool) {
	trimmed := strings.TrimPrefix(p, "/preview/")
	if trimmed == p {
		return "", 0, "", false
	}
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", 0, "", false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil || port < 1 || port > 65535 {
		return "", 0, "", false
	}
	rest = "/"
	if len(parts) == 3 {
		rest = "/" + parts[2]
	}
	return parts[0], port, rest, true
}
