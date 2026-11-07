package orchestrator

// What a visitor to a share link meets: the host routing, the warning page,
// the limits, and the reverse proxy into the tunnel.

import (
	"context"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
)

const (
	// shareAckPath is reserved on every share host; an app cannot serve it.
	shareAckPath   = "/.pilldrops-share/continue"
	shareAckCookie = "pd_share_ack"
)

// shareAckName is the acknowledgement cookie's name. Over https the __Host-
// prefix makes browsers refuse it from a sibling share that names a Domain.
func shareAckName() string {
	if shareScheme() == "https" {
		return "__Host-" + shareAckCookie
	}
	return shareAckCookie
}

var shareWarningPage = template.Must(template.New("warning").Parse(`<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Shared app</title>
<body style="font:16px system-ui;max-width:32rem;margin:4rem auto;padding:0 1rem">
<h1>You are about to open a shared app</h1>
<p>This link leads to an app running on a PillDrops user's own machine. PillDrops does not run or review it.</p>
<p><strong>Do not enter passwords or payment details unless you know who sent you this link.</strong></p>
<form method="post" action="{{.Action}}"><input type="hidden" name="next" value="{{.Next}}">
<button type="submit">Continue</button></form>
{{if .Contact}}<p><small>Report abuse: {{.Contact}}</small></p>{{end}}
</body>`))

var sharePlainPage = template.Must(template.New("plain").Parse(`<!doctype html>
<meta charset="utf-8"><title>{{.Title}}</title>
<body style="font:16px system-ui;max-width:32rem;margin:4rem auto;padding:0 1rem">
<h1>{{.Title}}</h1><p>{{.Message}}</p></body>`))

func writeSharePage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = sharePlainPage.Execute(w, map[string]string{"Title": title, "Message": message})
}

// shareFront answers everything under the share domain and passes the rest on.
// Outermost, because hostIngress returns early when no sandbox domain is set.
func (s *Server) shareFront(next http.Handler) http.Handler {
	domain := shareHost()
	if domain == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.TrimSuffix(proxy.NormalizeHost(r.Host), ".")
		if host != domain && !strings.HasSuffix(host, "."+domain) {
			next.ServeHTTP(w, r)
			return
		}
		if !ShareIngressEnabled() {
			writeSharePage(w, http.StatusServiceUnavailable, "Link unavailable",
				"This server does not handle share links. Its operator should route this domain to the share ingress.")
			return
		}
		if host == domain {
			s.serveShareApex(w, r)
			return
		}
		name, ok := proxy.ParseSlugHost(host, domain)
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.serveShareLink(w, r, name)
	})
}

func (s *Server) serveShareLink(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("X-Robots-Tag", "noindex")
	t := s.shares.get(name)
	if t == nil {
		s.serveShareOffline(w, r, name)
		return
	}
	if t.expired(s.now()) {
		t.end("expired")
		writeSharePage(w, http.StatusNotFound, "Link ended", "This share link has ended.")
		return
	}
	if !t.limiter.Allow() {
		w.Header().Set("Retry-After", "1")
		writeSharePage(w, http.StatusTooManyRequests, "Too many requests", "This link is getting too many requests. Try again shortly.")
		return
	}
	if r.URL.Path == shareAckPath && r.Method == http.MethodPost {
		// A page on another site must not be able to acknowledge for the visitor.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			writeShareWarning(w, "/")
			return
		}
		acknowledgeShare(w, r)
		return
	}
	if isShareNavigation(r) && !hasShareAck(r) {
		writeShareWarning(w, r.URL.RequestURI())
		return
	}
	t.proxy.ServeHTTP(w, r)
}

// serveShareOffline explains a name with no tunnel here. Rate limited per
// address, since each answer costs a database read and the names are public.
func (s *Server) serveShareOffline(w http.ResponseWriter, r *http.Request, name string) {
	if s.previewLimit != nil && s.previewLimit.rps > 0 && !s.previewLimit.allow(httpx.ClientIP(r)) {
		w.Header().Set("Retry-After", "1")
		writeSharePage(w, http.StatusTooManyRequests, "Too many requests", "Try again shortly.")
		return
	}
	st := s.st.Load()
	if st == nil {
		writeSharePage(w, http.StatusServiceUnavailable, "Link unavailable", "Try again shortly.")
		return
	}
	sh, ok, err := st.ShareByName(r.Context(), name)
	if err != nil {
		writeSharePage(w, http.StatusServiceUnavailable, "Link unavailable", "Try again shortly.")
		return
	}
	if !ok {
		writeSharePage(w, http.StatusNotFound, "Link not found", "No share link has this address.")
		return
	}
	switch sh.State(s.now().Unix()) {
	case "open":
		writeSharePage(w, http.StatusServiceUnavailable, "App offline",
			"The person sharing this app is not connected right now. Try again shortly.")
	case "blocked":
		writeSharePage(w, http.StatusGone, "Link removed", "This share link has been removed.")
	default:
		writeSharePage(w, http.StatusNotFound, "Link ended", "This share link has ended.")
	}
}

// isShareNavigation is a browser opening a page. Webhooks and curl send no
// fetch metadata, and subresources of a page already acknowledged have the cookie.
func isShareNavigation(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe", "frame", "object", "embed":
		return true
	}
	return false
}

func hasShareAck(r *http.Request) bool {
	c, err := r.Cookie(shareAckName())
	return err == nil && c.Value == "1"
}

func writeShareWarning(w http.ResponseWriter, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The button must not be clickable through someone else's frame.
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(http.StatusOK)
	_ = shareWarningPage.Execute(w, map[string]string{
		"Action": shareAckPath, "Next": next, "Contact": os.Getenv("SHARE_ABUSE_CONTACT"),
	})
}

// acknowledgeShare sets the cookie, with no Domain so it covers this link only.
func acknowledgeShare(w http.ResponseWriter, r *http.Request) {
	next := "/"
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	// ParseForm, not PostFormValue: that would parse multipart bodies to disk.
	if err := r.ParseForm(); err == nil {
		next = safeShareNext(r.PostForm.Get("next"))
	}
	http.SetCookie(w, &http.Cookie{
		Name: shareAckName(), Value: "1", Path: "/", MaxAge: 24 * 60 * 60,
		HttpOnly: true, Secure: shareScheme() == "https", SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// safeShareNext keeps only a same-site path and query. Browsers drop tabs and
// newlines and read a backslash as a slash, so any of them could turn "/x" into "//host".
func safeShareNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.ContainsFunc(next, func(r rune) bool { return r == '\\' || unicode.IsControl(r) }) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return u.RequestURI()
}

// withoutShareAck removes our cookie and leaves the app's exactly as sent.
func withoutShareAck(h http.Header) {
	values := h.Values("Cookie")
	h.Del("Cookie")
	for _, line := range values {
		var kept []string
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if part != "" && !strings.HasPrefix(part, shareAckName()+"=") {
				kept = append(kept, part)
			}
		}
		if len(kept) > 0 {
			h.Add("Cookie", strings.Join(kept, "; "))
		}
	}
}

// withoutAppAckCookie drops any Set-Cookie from the app that browsers would
// store as the acknowledgement, so the app cannot skip the warning for a visitor.
func withoutAppAckCookie(h http.Header) {
	values := h.Values("Set-Cookie")
	h.Del("Set-Cookie")
	for _, line := range values {
		name, _, _ := strings.Cut(strings.SplitN(line, ";", 2)[0], "=")
		name = strings.TrimSpace(name)
		if strings.EqualFold(name, shareAckCookie) || strings.EqualFold(name, "__Host-"+shareAckCookie) {
			continue
		}
		h.Add("Set-Cookie", line)
	}
}

// newShareProxy builds a tunnel's reverse proxy. Each pooled connection is one
// data WebSocket, and a dead one is retried by the transport as usual.
func (s *Server) newShareProxy(t *shareTunnel) (*httputil.ReverseProxy, *http.Transport) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return t.dial(ctx)
		},
		DisableCompression:  true,
		MaxIdleConnsPerHost: 8,
		// Under Node's 5 s keep-alive: a POST sent on a connection it is closing cannot be retried.
		IdleConnTimeout: 4 * time.Second,
	}
	host := "localhost:" + strconv.Itoa(t.localPort)
	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Rewrite has already removed every inbound X-Forwarded-* header.
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = host
			// Dev servers such as Vite reject a host they do not know.
			pr.Out.Host = host
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", shareScheme())
			pr.Out.Header.Set("X-Forwarded-For", httpx.ClientIP(pr.In))
			withoutShareAck(pr.Out.Header)
		},
		ModifyResponse: func(resp *http.Response) error {
			withoutAppAckCookie(resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errShareBusy) {
				w.Header().Set("Retry-After", "1")
				writeSharePage(w, http.StatusTooManyRequests, "Too many connections",
					"This link has too many open connections. Try again shortly.")
				return
			}
			writeSharePage(w, http.StatusBadGateway, "App not responding",
				"The shared app is not responding. It may have stopped or still be starting.")
		},
	}, transport
}
