package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// testWebhookURL is a valid target for tests whose subject is not the URL.
const testWebhookURL = "https://192.0.2.1/hook"

type sentWebhook struct {
	url, secret, deliveryID string
	body                    map[string]any
}

// stubWebhookSend replaces the sender, answering each call with the next status.
func stubWebhookSend(t *testing.T, statuses ...int) *[]sentWebhook {
	t.Helper()
	var sent []sentWebhook
	orig := webhookSend
	t.Cleanup(func() { webhookSend = orig })
	webhookSend = func(_ context.Context, url, secret, deliveryID string, body []byte) (int, error) {
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("delivery body is not JSON: %v", err)
		}
		sent = append(sent, sentWebhook{url, secret, deliveryID, decoded})
		return statuses[len(sent)-1], nil
	}
	return &sent
}

func TestSandboxLifecycleEventsReachAnOrgWebhook(t *testing.T) {
	srv, key, id, _, api := newCronSandboxWithAPI(t)
	ctx := t.Context()

	if code, out := do(t, "POST", srv+"/v1/webhooks", key,
		`{"url":"`+testWebhookURL+`","events":["sandbox.launched"]}`); code != http.StatusBadRequest {
		t.Fatalf("an unknown event was accepted: %d %v", code, out)
	}
	code, out := do(t, "POST", srv+"/v1/webhooks", key,
		`{"url":"`+testWebhookURL+`","events":["sandbox.ended","sandbox.out_of_memory"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create hook = %d %v", code, out)
	}
	hookID := out["webhook"].(map[string]any)["id"].(string)
	sent := stubWebhookSend(t, http.StatusOK, http.StatusOK, http.StatusBadGateway)

	record := func(event string, at int64) {
		t.Helper()
		if err := api.st.RecordEvent(ctx, store.EventRecord{
			OrgID: "default", SandboxID: id, Event: event, Template: "python", At: at,
		}); err != nil {
			t.Fatalf("record %s: %v", event, err)
		}
	}
	now := time.Now().Unix()
	record("paused", now) // not subscribed
	record("oom", now)    // the sandbox lives on: an out-of-memory kill, not an end

	if err := api.st.RecordHistory(ctx, store.HistoryRecord{
		ID: id, OrgID: "default", Template: "python", ExitState: "oom",
		CreatedAt: now - 90, DestroyedAt: now + 1, DurationSecs: 91, VCPU: 2, MemMiB: 512,
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}
	// Destroying the sandbox must not take the org hook's queued deliveries with it.
	if err := api.st.DeleteAgentWebhooksForSandbox(ctx, id); err != nil {
		t.Fatalf("forget sandbox hooks: %v", err)
	}
	record("oom", now+1)

	if code, out := do(t, "POST", srv+"/v1/webhooks/"+hookID+"/toggle", key, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("pause = %d %v", code, out)
	}
	record("destroyed", now+2) // paused hooks queue nothing
	if code, out := do(t, "POST", srv+"/v1/webhooks/"+hookID+"/toggle", key, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("resume = %d %v", code, out)
	}

	api.deliverDueWebhooks(ctx, time.Now(), nil)
	if len(*sent) != 2 {
		t.Fatalf("sent %d deliveries, want 2: %+v", len(*sent), *sent)
	}
	byType := map[string]map[string]any{}
	for _, s := range *sent {
		if s.body["id"] != s.deliveryID || s.body["sandbox"].(map[string]any)["id"] != id {
			t.Errorf("delivery body = %v", s.body)
		}
		byType[s.body["type"].(string)] = s.body
	}
	if byType["sandbox.out_of_memory"] == nil {
		t.Errorf("no out-of-memory delivery: %v", byType)
	}
	ended := byType["sandbox.ended"]
	if ended == nil {
		t.Fatalf("no ended delivery: %v", byType)
	}
	if data, _ := ended["data"].(map[string]any); data["reason"] != "out_of_memory" || data["ran_seconds"] != float64(91) {
		t.Errorf("ended data = %v", ended["data"])
	}

	_, list := do(t, "GET", srv+"/v1/webhooks/deliveries", key, "")
	if rows, _ := list["deliveries"].([]any); len(rows) != 2 {
		t.Errorf("deliveries = %v", list)
	}
	if code, out := do(t, "POST", srv+"/v1/webhooks/"+hookID+"/test", key, ""); code != http.StatusOK ||
		out["ok"] != false || out["retryable"] != true || (*sent)[2].body["type"] != "webhook.test" {
		t.Errorf("test send = %d %v", code, out)
	}
	if code, _ := do(t, "DELETE", srv+"/v1/webhooks/"+hookID, key, ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
}
