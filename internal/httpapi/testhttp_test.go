package httpapi

import (
	"io"
	"net/http"
	"testing"
)

// getText is an authenticated GET returning the raw body.
func getText(t *testing.T, url, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
