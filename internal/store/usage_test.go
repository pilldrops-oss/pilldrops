package store

import "testing"

// Both the single-row and batch paths must stamp node_class.
func TestUsageByClassSplitsConsumptionByHardware(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if err := st.RecordUsage(ctx, "sb-1", "acme", UsageDelta{
		VCPUSeconds: 100, RunSeconds: 50, NodeClass: "gpu-a100",
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := st.RecordUsageBatch(ctx, []UsageEntry{
		{SandboxID: "sb-2", OrgID: "acme", Delta: UsageDelta{VCPUSeconds: 20, NodeClass: "gpu-a100"}},
		{SandboxID: "sb-3", OrgID: "acme", Delta: UsageDelta{VCPUSeconds: 7, NodeClass: "cpu-small"}},
	}); err != nil {
		t.Fatalf("RecordUsageBatch: %v", err)
	}

	got, err := st.UsageByClass(ctx, "acme", 0)
	if err != nil {
		t.Fatalf("UsageByClass: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 classes, got %d: %+v", len(got), got)
	}
	if got[0].NodeClass != "cpu-small" || got[0].VCPUSeconds != 7 {
		t.Errorf("cpu-small row wrong: %+v", got[0])
	}
	if got[1].NodeClass != "gpu-a100" || got[1].VCPUSeconds != 120 {
		t.Errorf("gpu-a100 row wrong: %+v", got[1])
	}
	if got[1].RunSeconds != 50 {
		t.Errorf("run_seconds did not survive the split: %+v", got[1])
	}
}

func TestUsageByClassKeepsUnclassifiedRows(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	if err := st.RecordUsage(ctx, "sb-1", "acme", UsageDelta{VCPUSeconds: 5}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	got, err := st.UsageByClass(ctx, "acme", 0)
	if err != nil {
		t.Fatalf("UsageByClass: %v", err)
	}
	if len(got) != 1 || got[0].NodeClass != "" || got[0].VCPUSeconds != 5 {
		t.Fatalf("unclassified usage was dropped or relabelled: %+v", got)
	}
}

func TestUsageByClassHonoursTheWindow(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	if err := st.RecordUsage(ctx, "sb-1", "acme", UsageDelta{VCPUSeconds: 9, NodeClass: "gpu"}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	got, err := st.UsageByClass(ctx, "acme", 1<<40)
	if err != nil {
		t.Fatalf("UsageByClass: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rows outside the window were counted: %+v", got)
	}
}
