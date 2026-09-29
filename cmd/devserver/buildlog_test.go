package main

import (
	"strings"
	"testing"
)

// A live credential landing in the research table would make it a place
// secrets accumulate.
func TestScrubSecretsRedactsRealCredentialShapes(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string // the substring that must NOT survive
	}{
		{"anthropic", "build a bot, my key is sk-ant-api03-AbCdEf0123456789xyz", "sk-ant-api03-AbCdEf0123456789xyz"},
		{"openai", "use sk-proj-9f8e7d6c5b4a39281706abcdef", "sk-proj-9f8e7d6c5b4a39281706abcdef"},
		{"github pat", "clone with ghp_16CharsMinimumAbcdefghij", "ghp_16CharsMinimumAbcdefghij"},
		{"gitlab pat", "glpat-AbCdEfGh12345678IjKl", "glpat-AbCdEfGh12345678IjKl"},
		{"slack", "post via xoxb-123456789012-abcdefghijkl", "xoxb-123456789012-abcdefghijkl"},
		{"pilldrops", "my key sf_d086d4fee0ed2013dff2783dc8950095", "sf_d086d4fee0ed2013dff2783dc8950095"},
		{"stripe", "charge with sk_live_abcdef1234567890", "sk_live_abcdef1234567890"},
		{"aws", "AKIAIOSFODNN7EXAMPLE is the id", "AKIAIOSFODNN7EXAMPLE"},
		{"google", "AIzaSyD-1234567890abcdefghijklmnopqrs", "AIzaSyD-1234567890abcdefghijklmnopqrs"},
		{"huggingface", "hf_AbCdEfGhIjKlMnOpQrStUvWxYz1234", "hf_AbCdEfGhIjKlMnOpQrStUvWxYz1234"},
		// No labelling word in the input on purpose, so this fails if the
		// dedicated Telegram pattern is deleted.
		{"telegram bot token", "hermes agent connected to 123456789:AAHkQ7dRlP-abcdefghijklmnopqrstuvwx", "123456789:AAHkQ7dRlP-abcdefghijklmnopqrstuvwx"},
		{"jwt", "session eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "eyJhbGciOiJIUzI1NiJ9"},
		{"labelled bearer", "set Authorization: Bearer abcdefghijklmnopqrstuv", "abcdefghijklmnopqrstuv"},
		{"labelled api key", "api_key = 8f3b2c1d9e0a7b6c5d4e3f2a1b0c9d8e", "8f3b2c1d9e0a7b6c5d4e3f2a1b0c9d8e"},
		{"bare hex", "use 5f4dcc3b5aa765d61d8327deb882cf99 please", "5f4dcc3b5aa765d61d8327deb882cf99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubSecrets(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("credential survived scrubbing.\n in: %q\nout: %q\nleaked: %q", tc.in, got, tc.secret)
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("nothing was redacted, so the pattern never fired.\n in: %q\nout: %q", tc.in, got)
			}
		})
	}
}

// The scrub is greedy but must still leave a readable request behind.
func TestScrubSecretsKeepsOrdinaryRequestsIntact(t *testing.T) {
	cases := []string{
		"i want to create my own hermes agent with telegram connect",
		"i want claude code",
		"a mobile testing rig with adb and a real chrome",
		"python api with postgres and redis",
		"claude code on my repo github.com/acme/widgets",
		"set up node 22 with pnpm",
	}
	for _, in := range cases {
		if got := scrubSecrets(in); got != in {
			t.Errorf("ordinary request was altered — operators would read a mangled backlog.\n in: %q\nout: %q", in, got)
		}
	}
}

func TestNormalisePromptCollapsesWhitespaceSoRepeatsGroup(t *testing.T) {
	// Equivalent requests must normalise equal or the "most asked for" ranking splits.
	a := normalisePrompt("  i want   openclaw\n with telegram  ")
	b := normalisePrompt("i want openclaw with telegram")
	if a != b {
		t.Fatalf("equivalent requests did not normalise equal:\n%q\n%q", a, b)
	}
}

func TestNormalisePromptTruncatesAndStaysValidUTF8(t *testing.T) {
	// Multi-byte on purpose: a byte-wise cut would store invalid UTF-8.
	in := strings.Repeat("日", maxPromptLen+500)
	got := normalisePrompt(in)
	if n := len([]rune(got)); n > maxPromptLen {
		t.Fatalf("prompt not truncated: %d runes", n)
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("truncation produced an invalid rune at byte %d", i)
		}
	}
}

func TestBuildOutcomesAreClosed(t *testing.T) {
	// A client cannot invent outcome values that would fragment the grouping.
	for _, ok := range []string{buildOutcomeMatched, buildOutcomeUnmatched, buildOutcomeLaunched, buildOutcomeFailed} {
		if !buildOutcomes[ok] {
			t.Errorf("%q missing from buildOutcomes", ok)
		}
	}
	for _, bad := range []string{"", "MATCHED", "launched ", "whatever"} {
		if buildOutcomes[bad] {
			t.Errorf("%q should not be an accepted outcome", bad)
		}
	}
}
