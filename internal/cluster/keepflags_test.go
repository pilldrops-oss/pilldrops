package cluster

import (
	"testing"
	"time"
)

func TestKeepFlagsRestoresACordonAcrossARestart(t *testing.T) {
	saved := map[string]SavedNodeFlags{}
	save := func(f SavedNodeFlags) {
		if !f.Cordoned && !f.Draining {
			delete(saved, f.NodeID)
			return
		}
		saved[f.NodeID] = f
	}
	beat := func(r *Registry, id string) {
		t.Helper()
		if err := r.nodes.Upsert(NodeUpsert{ID: id}, time.Now()); err != nil {
			t.Fatalf("heartbeat %s: %v", id, err)
		}
	}
	restart := func() *Registry {
		r := NewRegistry()
		rows := make([]SavedNodeFlags, 0, len(saved))
		for _, f := range saved {
			rows = append(rows, f)
		}
		r.KeepFlags(rows, save)
		return r
	}

	first := restart()
	beat(first, "cordoned")
	beat(first, "removed")
	beat(first, "back")
	first.SetCordon("cordoned", true)
	first.SetDrain("removed", true)
	first.SetCordon("back", true)
	first.SetCordon("back", false)
	first.Remove("removed")

	second := restart()
	for _, id := range []string{"cordoned", "removed", "back"} {
		beat(second, id)
	}
	flags := func(id string) *Node {
		t.Helper()
		n, ok, err := second.nodes.Get(id)
		if err != nil || !ok {
			t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
		}
		return n
	}
	if !flags("cordoned").Cordoned {
		t.Error("the cordon was lost in the restart")
	}
	if flags("removed").Draining {
		t.Error("a removed node's drain came back")
	}
	if flags("back").Cordoned {
		t.Error("an uncordoned node came back cordoned")
	}
}
