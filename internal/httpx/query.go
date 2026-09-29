package httpx

import (
	"net/http"
	"strconv"
)

// QueryInt reads a positive integer query parameter, falling back to def when
// it is absent, unparseable or not positive.
//
// Both tiers wrote this out sixteen times, and five of those copies left the
// "is it positive" half off — so limit=-1 reached the store on /v1/history and
// /v1/admin/waitlist while the same value was refused on /v1/events. A limit
// and an offset are the only things this parses, and a negative one is
// meaningless at both.
func QueryInt(r *http.Request, name string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && n > 0 {
		return n
	}
	return def
}
