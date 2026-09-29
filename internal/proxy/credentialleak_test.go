package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fixedResolver string

func (f fixedResolver) PreviewTarget(context.Context, string, int, string) (string, error) {
	return string(f), nil
}

// The tokens the preview gate reads must not reach the customer's application,
// where they land in its access log and anything else it records.
//
// Against the old code this fails on the first assertion: the whole query was
// forwarded verbatim, credential values included.
func TestPreviewCredentialsAreStrippedBeforeTheApp(t *testing.T) {
	var gotQuery string
	app := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
	}))
	defer app.Close()

	h := Handler(fixedResolver(strings.TrimPrefix(app.URL, "http://")))
	req := httptest.NewRequest("GET",
		"/preview/sb-1/8000/app?token=browser-secret&ptok=hop-secret&page=2", nil)
	req = requestWithPreviewOrg(req, "default")
	h(httptest.NewRecorder(), req)

	for _, leaked := range []string{"browser-secret", "hop-secret", "token=", "ptok="} {
		if strings.Contains(gotQuery, leaked) {
			t.Errorf("the app was sent %q in %q", leaked, gotQuery)
		}
	}
	if !strings.Contains(gotQuery, "page=2") {
		t.Errorf("the app's own query parameters were dropped: %q", gotQuery)
	}
}

func TestOnlyThePreviewCredentialsAreDropped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"a=1&b=2", "a=1&b=2"},
		{"token=x", ""},
		{"ptok=x", ""},
		{"a=1&ptok=x&token=y", "a=1"},
		// A name that merely contains one of them stays: an app may well have
		// its own ?token_hint=.
		{"token_hint=1", "token_hint=1"},
		// Unparseable and still carrying a token: the app gets nothing rather
		// than a credential. A query with no "tok" in it anywhere cannot be
		// carrying either name, so it is passed through untouched.
		{"%zz&token=x", ""},
		{"%zz", "%zz"},
	} {
		if got := withoutPreviewCredentials(tc.in); got != tc.want {
			t.Errorf("withoutPreviewCredentials(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
