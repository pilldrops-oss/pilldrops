// Command pilldrops-join redeems a join token at the orchestrator and writes
// the node's /etc/sandboxd.env. The token comes from stdin, JOIN_TOKEN_FILE
// or JOIN_TOKEN, in that order.
//
//	ORCHESTRATOR_URL=https://orch NODE_ZONE=par1 \
//	  ADVERTISE_URL=http://<public-ip>:18080 pilldrops-join   # token on stdin
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/nodejoin"
)

// readToken resolves the join token, most private source first. A terminal
// stdin is not read, since that would hang a run that passes the token by env.
func readToken() (string, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	}
	if f := os.Getenv("JOIN_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("read JOIN_TOKEN_FILE: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv("JOIN_TOKEN")), nil
}

func main() {
	orch := os.Getenv("ORCHESTRATOR_URL")
	advertise := os.Getenv("ADVERTISE_URL")
	zone := os.Getenv("NODE_ZONE")
	if zone == "" {
		zone = "par1"
	}
	token, err := readToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pilldrops-join:", err)
		os.Exit(2)
	}
	if orch == "" || token == "" || advertise == "" {
		fmt.Fprintln(os.Stderr,
			"pilldrops-join: set ORCHESTRATOR_URL and ADVERTISE_URL, and supply the join token\n"+
				"  on stdin (preferred):  echo \"$TOKEN\" | pilldrops-join\n"+
				"  or JOIN_TOKEN_FILE=/path/to/token\n"+
				"  or JOIN_TOKEN=… (avoid: readable in /proc/<pid>/environ)")
		os.Exit(2)
	}
	cfg, err := nodejoin.FetchConfig(context.Background(), orch, token, advertise, zone)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pilldrops-join:", err)
		os.Exit(1)
	}
	out := os.Getenv("SANDBOXD_ENV_FILE")
	if out == "" {
		out = "/etc/sandboxd.env"
	}
	if err := os.WriteFile(out, []byte(nodejoin.RenderEnv(cfg)), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "pilldrops-join: write env:", err)
		os.Exit(1)
	}
	fmt.Printf("joined as %s (zone %s) — wrote %s\n", cfg.NodeID, cfg.Zone, out)
}
