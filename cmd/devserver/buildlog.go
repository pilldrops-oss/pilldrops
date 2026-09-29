package main

import (
	"regexp"
	"strings"
)

// Build prompts are recorded so operators can see what users ask for and
// whether it was served. Unmatched rows are the backlog, in users' own words.

// Outcomes are short stable strings because they are grouped in SQL and
// rendered as-is in the console.
const (
	buildOutcomeMatched   = "matched"   // a named agent was recognised; nothing launched yet
	buildOutcomeUnmatched = "unmatched" // could not be served; the backlog
	buildOutcomeLaunched  = "launched"  // a sandbox was actually created
	buildOutcomeFailed    = "failed"    // launch was attempted and errored
)

var buildOutcomes = map[string]bool{
	buildOutcomeMatched:   true,
	buildOutcomeUnmatched: true,
	buildOutcomeLaunched:  true,
	buildOutcomeFailed:    true,
}

// maxPromptLen is long enough for a real sentence and short enough that the
// endpoint cannot be used to stuff the database.
const maxPromptLen = 2000

// secretish matches the shapes credentials arrive in. Deliberately greedy: a
// false positive costs one redacted word, a false negative writes a live key
// to a table operators browse.
var secretish = []*regexp.Regexp{
	// Vendor-prefixed keys: sk-, gh*_, xox*-, sf_, pk/sk_live, AKIA, AIza, hf_, glpat-.
	regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`(?i)\bgh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`(?i)\bxox[baprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`(?i)\bsf_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`(?i)\b[ps]k_(live|test)_[A-Za-z0-9]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{12,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{20,}`),
	regexp.MustCompile(`(?i)\bhf_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`(?i)\bglpat-[A-Za-z0-9_\-]{16,}`),
	// Telegram bot tokens: 123456789:AA…
	regexp.MustCompile(`\b\d{6,}:[A-Za-z0-9_\-]{30,}`),
	// A JWT, which is what a pasted session token looks like.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`),
	// "Authorization: Bearer <anything>" / "token: <anything>".
	regexp.MustCompile(`(?i)\b(bearer|authorization|token|api[_\- ]?key|password|secret)\b\s*[:=]?\s*\S{12,}`),
	// A bare long hex or base64 run is usually a credential; 32 is the shortest
	// length that skips ordinary words.
	regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9+/]{40,}={0,2}\b`),
}

// scrubSecrets replaces anything credential-shaped with a marker. Runs
// server-side so no client can skip it.
func scrubSecrets(s string) string {
	for _, re := range secretish {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// normalisePrompt collapses whitespace (so repeats group together), scrubs
// secrets, then truncates.
func normalisePrompt(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	s = scrubSecrets(s)
	if len(s) > maxPromptLen {
		// Rune-safe truncation: cutting mid-rune would store invalid UTF-8.
		r := []rune(s)
		if len(r) > maxPromptLen {
			r = r[:maxPromptLen]
		}
		s = string(r)
	}
	return s
}
