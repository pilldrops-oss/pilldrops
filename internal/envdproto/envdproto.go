// Package envdproto is the envd capability level, shared by the guest agent that
// reports it and the node that gates features on it. It imports nothing, so
// cmd/envd can link it.
//
// The level is one integer, not a semver: the node never asks "which release",
// only "can this guest do X". Bump Protocol whenever envd gains an RPC or a
// behaviour the node has to know about before using it, and add the constant
// naming what that level brought.
package envdproto

import (
	"bytes"
	"strconv"
	"strings"
)

// Protocol is the level this envd build speaks.
const Protocol = 3

// Tag is what envd reports as its Health version, and the literal a node scans
// a template's baked binary for. It must spell Protocol; a test pins that.
const Tag = "envd-protocol=3"

const tagPrefix = "envd-protocol="

// What each level added. Level 1 is every envd built before levels existed.
const (
	// VsockCredential: envd binds with no boot credential and takes its token
	// hash over vsock (SETHASH), which is what a golden snapshot VM needs.
	VsockCredential = 2
	// PtyCommand: the terminal WebSocket (/ws) takes cmd, cwd, cols and rows,
	// and closes with "exit=<code>". `envd watch <dir>` streams file events as
	// JSON lines. Both arrived together.
	PtyCommand   = 3
	WatchCommand = 3
)

// legacyCredentialMarker is the SETHASH command string. Binaries built after the
// vsock credential channel and before Tag existed carry it but no tag.
var legacyCredentialMarker = []byte("SETHASH")

// Parse reads a level from a Health version string. Anything without a tag,
// including the "1.0.0" every envd used to report, is level 1.
func Parse(version string) int {
	if n, ok := strings.CutPrefix(version, tagPrefix); ok {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			return v
		}
	}
	return 1
}

// Scan reads the level from an envd binary's bytes.
func Scan(bin []byte) int {
	if i := bytes.Index(bin, []byte(tagPrefix)); i >= 0 {
		rest := bin[i+len(tagPrefix):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if v, err := strconv.Atoi(string(rest[:end])); err == nil && v > 0 {
			return v
		}
	}
	if bytes.Contains(bin, legacyCredentialMarker) {
		return VsockCredential
	}
	return 1
}
