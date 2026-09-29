package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// A runtime that cannot say which envd its guest runs must refuse a watch as
// unsupported (a 501), not start a command an old envd would mistake for a
// second server.
func TestWatchDirRefusedWithoutAKnownEnvdLevel(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := context.Background()
	sb, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "default"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = d.WatchDir(ctx, sb.ID, "/tmp", time.Second, func([]byte) { t.Error("an event from a refused watch") })
	if !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("err = %v, want rt.ErrUnsupported", err)
	}
}
