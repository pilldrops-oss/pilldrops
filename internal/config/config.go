// Package config is the one place environment configuration is read, recorded
// and validated. Values are read at point of use; an invalid value is recorded
// rather than swallowed so a binary can fail fast at startup.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one resolved configuration value.
type Entry struct {
	Key string
	// Raw is what the environment held ("" when unset).
	Raw string
	// Value is what was actually used, rendered for display.
	Value string
	// Set reports whether the environment supplied a value at all.
	Set bool
	// Invalid reports that Raw was present but could not be parsed, so the default was used.
	Invalid bool
	// Secret marks a value that must never be logged.
	Secret bool
}

var (
	mu      sync.Mutex
	entries = map[string]Entry{}
)

func record(e Entry) {
	e.Secret = IsSecretKey(e.Key)
	mu.Lock()
	entries[e.Key] = e
	mu.Unlock()
}

// Int reads an integer, falling back to def when unset. A present-but-
// unparseable value is recorded as invalid.
func Int(key string, def int) int {
	raw, set := os.LookupEnv(key)
	if !set || raw == "" {
		record(Entry{Key: key, Value: strconv.Itoa(def)})
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		record(Entry{Key: key, Raw: raw, Value: strconv.Itoa(def), Set: true, Invalid: true})
		return def
	}
	record(Entry{Key: key, Raw: raw, Value: strconv.Itoa(n), Set: true})
	return n
}

// Str reads a string, falling back to def when unset.
func Str(key, def string) string {
	raw, set := os.LookupEnv(key)
	if !set || raw == "" {
		record(Entry{Key: key, Value: def})
		return def
	}
	record(Entry{Key: key, Raw: raw, Value: raw, Set: true})
	return raw
}

// Bool reads a boolean: 1/0, true/false, yes/no, on/off. Anything else is
// recorded as invalid rather than read as false.
func Bool(key string, def bool) bool {
	raw, set := os.LookupEnv(key)
	if !set || raw == "" {
		record(Entry{Key: key, Value: strconv.FormatBool(def)})
		return def
	}
	v, ok := ParseBool(raw)
	if !ok {
		record(Entry{Key: key, Raw: raw, Value: strconv.FormatBool(def), Set: true, Invalid: true})
		return def
	}
	record(Entry{Key: key, Raw: raw, Value: strconv.FormatBool(v), Set: true})
	return v
}

// ParseBool is Bool's parse without the environment or the recording. ok is
// false for an unrecognised value, so a caller can tell explicitly-off from unparseable.
func ParseBool(raw string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "y":
		return true, true
	case "0", "false", "no", "off", "n":
		return false, true
	}
	return false, false
}

// Duration reads a Go duration string ("30s", "10m"). An unparseable value is
// recorded as invalid.
func Duration(key string, def time.Duration) time.Duration {
	raw, set := os.LookupEnv(key)
	if !set || raw == "" {
		record(Entry{Key: key, Value: def.String()})
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		record(Entry{Key: key, Raw: raw, Value: def.String(), Set: true, Invalid: true})
		return def
	}
	record(Entry{Key: key, Raw: raw, Value: d.String(), Set: true})
	return d
}

// Invalid returns every variable whose value was present but unusable, in key order.
func Invalid() []Entry {
	mu.Lock()
	defer mu.Unlock()
	var out []Entry
	for _, e := range entries {
		if e.Invalid {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Validate returns an error naming every invalid value, or nil. Call it at startup and fail fast.
func Validate() error {
	bad := Invalid()
	if len(bad) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("invalid configuration:")
	for _, e := range bad {
		fmt.Fprintf(&b, "\n  %s=%q could not be parsed; using %s instead",
			e.Key, redact(e), e.Value)
	}
	return fmt.Errorf("%s", b.String())
}

// Resolved returns every value read so far, in key order, with secrets redacted.
func Resolved() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Secret {
			e.Raw, e.Value = redactedMarker(e.Set), redactedMarker(e.Set)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func redact(e Entry) string {
	if e.Secret {
		return redactedMarker(e.Set)
	}
	return e.Raw
}

func redactedMarker(set bool) string {
	if set {
		return "<set>"
	}
	return "<unset>"
}

// secretMarkers name the substrings that make a variable a credential.
// Over-redaction is deliberate; DSNs are included because they embed passwords.
var secretMarkers = []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "DSN", "DATABASE_URL", "REDIS_URL"}

// IsSecretKey reports whether a variable's value must never be logged.
func IsSecretKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, m := range secretMarkers {
		if strings.Contains(upper, m) {
			return true
		}
	}
	return false
}

// Reset clears the recorded entries. For tests only.
func Reset() {
	mu.Lock()
	entries = map[string]Entry{}
	mu.Unlock()
}

// LogResolved prints one line per setting at startup, secrets redacted. Gated on LOG_CONFIG.
func LogResolved() {
	if !Bool("LOG_CONFIG", false) {
		return
	}
	for _, e := range Resolved() {
		src := "default"
		if e.Set {
			src = "env"
		}
		slog.Info("config", "component", "config", "key", e.Key, "value", e.Value, "source", src)
	}
}
