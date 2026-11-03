//go:build linux

package vm

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
)

// A paused sandbox resumes into a fresh namespace built from its snapshot's
// rules, and the next pause writes the rules the resumed sandbox holds. If the
// resume drops them, the first cycle still looks confined, the second pause
// writes no rules and the second resume comes back with open egress; a fork of
// the resumed sandbox is open at once. Run like the other Live tests, with
// FC_NETNS support.
func TestLiveEgressSurvivesPauseResume(t *testing.T) {
	cfg, tmpl := liveCfg(t)
	cfg.EnvdAuth = true
	cfg.NetNS = true
	cfg.Templates = []string{tmpl}
	ctx := context.Background()
	m := liveManager(t, cfg)

	rules := &egress.Rules{Allow: []string{"203.0.113.7"}, Deny: []string{egress.AllTraffic}}
	sb, err := m.Create(ctx, CreateRequest{Template: tmpl, UserID: "live-test", Egress: rules})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := sb.ID
	defer func() { _ = m.Destroy(ctx, id) }()

	pause := func(when string) string {
		t.Helper()
		dir := t.TempDir()
		if err := m.SnapshotSandbox(ctx, id, dir); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		meta, err := readSnapMeta(dir)
		if err != nil {
			t.Fatalf("%s: read snapshot meta: %v", when, err)
		}
		if !reflect.DeepEqual(meta.Egress, rules) {
			t.Fatalf("%s wrote egress %+v, want %+v", when, meta.Egress, rules)
		}
		return dir
	}
	holds := func(who string) {
		t.Helper()
		m.mu.RLock()
		held := m.active[who].Egress
		m.mu.RUnlock()
		if !reflect.DeepEqual(held, rules) {
			t.Fatalf("%s holds egress %+v, want %+v", who, held, rules)
		}
		res, err := m.Exec(ctx, who, "curl -s -o /dev/null -w %{http_code} --max-time 5 http://1.1.1.1/", "", 15*time.Second)
		if err != nil {
			t.Fatalf("exec on %s: %v", who, err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != "000" {
			t.Fatalf("%s: a denied address answered %q; egress is open", who, got)
		}
	}

	if _, err := m.RestoreSandbox(ctx, id, pause("first pause"), 1, 1024); err != nil {
		t.Fatalf("resume: %v", err)
	}
	holds(id)

	childID := id + "-fork"
	if _, _, err := m.ForkSandbox(ctx, id, childID, 1, 1024); err != nil {
		t.Fatalf("fork: %v", err)
	}
	defer func() { _ = m.Destroy(ctx, childID) }()
	holds(childID)

	pause("second pause")
}
