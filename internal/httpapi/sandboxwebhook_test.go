package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
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

func TestASlowReceiverDoesNotHoldUpOtherHooks(t *testing.T) {
	srv, key, id, _, api := newCronSandboxWithAPI(t)
	ctx := t.Context()

	const slowURL, fastURL = "https://192.0.2.1/slow", "https://192.0.2.1/fast"
	for _, url := range []string{slowURL, fastURL} {
		if code, out := do(t, "POST", srv+"/v1/webhooks", key,
			`{"url":"`+url+`","events":["sandbox.paused"]}`); code != http.StatusCreated {
			t.Fatalf("create hook = %d %v", code, out)
		}
	}

	var slowCalls atomic.Int32
	release := make(chan struct{})
	sent := make(chan string, 8)
	orig := webhookSend
	t.Cleanup(func() { webhookSend = orig })
	webhookSend = func(_ context.Context, url, _, _ string, _ []byte) (int, error) {
		if url == slowURL {
			slowCalls.Add(1)
			<-release
		}
		sent <- url
		return http.StatusOK, nil
	}
	pause := func(at int64) {
		t.Helper()
		if err := api.st.RecordEvent(ctx, store.EventRecord{
			OrgID: "default", SandboxID: id, Event: "paused", Template: "python", At: at,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	awaitFast := func() {
		t.Helper()
		select {
		case url := <-sent:
			if url != fastURL {
				t.Fatalf("sent to %s while the slow receiver was still held", url)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the fast hook waited on the slow receiver")
		}
	}

	snd := &webhookSender{busy: map[string]bool{}}
	now := time.Now()
	pause(now.Unix())
	api.startDueWebhooks(ctx, time.Now(), nil, snd)
	awaitFast()

	// The slow hook is still in flight: a later round serves the fast hook and
	// leaves the slow hook's next delivery queued behind its first.
	pause(now.Unix() + 1)
	inFlight := func() int {
		snd.mu.Lock()
		defer snd.mu.Unlock()
		return len(snd.busy)
	}
	for inFlight() > 1 { // the fast worker has sent but may not have finished
		time.Sleep(time.Millisecond)
	}
	api.startDueWebhooks(ctx, time.Now(), nil, snd)
	awaitFast()
	if got := slowCalls.Load(); got != 1 {
		t.Errorf("the slow hook had %d deliveries in flight, want 1 at a time", got)
	}

	close(release)
	snd.wg.Wait()
	api.deliverDueWebhooks(ctx, time.Now(), nil)
	if got := len(sent); got != 2 {
		t.Errorf("the slow hook got %d deliveries after its receiver recovered, want 2", got)
	}
}

func TestWebhookCreateRefusesBodiesThatAreNotSmallJSON(t *testing.T) {
	srv, key, _, _, _ := newCronSandboxWithAPI(t)
	post := func(contentType, body string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", srv+"/v1/webhooks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	valid := `{"url":"` + testWebhookURL + `","events":["sandbox.ended"]}`
	padded := `{"url":"` + testWebhookURL + `","events":["sandbox.ended"],"pad":"` + strings.Repeat("x", webhookBodyLimit) + `"}`

	for name, c := range map[string]struct {
		contentType, body string
		want              int
	}{
		"plain text":          {"text/plain", valid, http.StatusUnsupportedMediaType},
		"form encoded":        {"application/x-www-form-urlencoded", "url=x", http.StatusUnsupportedMediaType},
		"over the limit":      {"application/json", padded, http.StatusRequestEntityTooLarge},
		"broken json":         {"application/json", `{"url":`, http.StatusBadRequest},
		"json with a charset": {"application/json; charset=utf-8", valid, http.StatusCreated},
		"no content type":     {"", valid, http.StatusCreated},
	} {
		if got := post(c.contentType, c.body); got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
	}
}
