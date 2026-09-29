// Package nodeboot holds the boot-time wiring shared by cmd/sandboxd and
// cmd/sandboxd-fleet: env helpers, runtime selection, production guards,
// bootstrap key, history retention, and the cluster heartbeat.
package nodeboot

import (
	"net"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/config"
)

// Env returns environment variable k, or def when unset. Goes through
// internal/config so an invalid value is reported at startup.
func Env(k, def string) string { return config.Str(k, def) }

// EnvInt returns environment variable k as an int, or def when unset or unparseable.
func EnvInt(k string, def int) int { return config.Int(k, def) }

// EnvBool reads a flag, accepting 1/true/yes/on and their negatives.
func EnvBool(k string, def bool) bool { return config.Bool(k, def) }

// LocalURL is the http URL this machine reaches a listen address on: ":8080"
// has no host, so it is localhost; "127.0.0.1:8080" keeps its host.
func LocalURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// TemplateList splits a comma-separated TEMPLATE value, defaulting to base.
func TemplateList(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = []string{"base"}
	}
	return out
}
