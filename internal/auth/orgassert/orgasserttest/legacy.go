// Package orgasserttest mints the pre-nonce org assertion shape so the
// compatibility path in orgassert.Verify can be tested. Kept out of orgassert
// so no production code can mint a replayable, node-unbound assertion.
package orgasserttest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
)

// SignLegacy produces "org|caller|exp|sig" bound to method and path only.
func SignLegacy(secret []byte, org, method, path string, now time.Time) string {
	payload := org + "|orchestrator|" + strconv.FormatInt(now.Add(orgassert.TTL).Unix(), 10)
	mac := hmac.New(sha256.New, deriveKey(secret))
	mac.Write([]byte(payload + "|" + strings.ToUpper(method) + "\n" + bindPath(path)))
	return payload + "|" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// deriveKey duplicates orgassert.deriveKey on purpose; drift makes the tests fail loudly.
func deriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("sandforge:org-override:v1"))
	return mac.Sum(nil)
}

func bindPath(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}
