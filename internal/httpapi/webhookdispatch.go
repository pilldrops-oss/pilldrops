package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log"
	"maps"
	"math/rand"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/webhook"
)

const (
	webhookPollInterval = 2 * time.Second
	// webhookClaimBatch deliveries can land on one hook and are then sent one
	// after another, so the lease must outlast a whole batch to a slow receiver
	// (DNS plus delivery timeout each).
	webhookClaimBatch = 10
	webhookLease      = 2 * time.Minute
	// webhookWorkers bounds how many hooks are sent to at once.
	webhookWorkers = 20
)

// webhookSend is a var only so tests can reach a local receiver, which the SSRF guard refuses.
var webhookSend = postSignedWebhook

// errWebhookUndeliverable marks a delivery that no retry can save.
var errWebhookUndeliverable = errors.New("webhook delivery cannot be made")

// StartWebhookDelivery begins sending queued webhook deliveries and returns a
// stop function. Started explicitly, like the cron sweeper.
func (s *Server) StartWebhookDelivery() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	go s.webhookDeliveryLoop(ctx)
	return cancel
}

// webhookSender sends to many hooks at once but to each hook one delivery at a
// time, so a slow receiver holds one worker and delays only its own hook.
type webhookSender struct {
	mu   sync.Mutex
	busy map[string]bool // hook ids with deliveries in flight
	wg   sync.WaitGroup
}

// webhookDeliveryLoop sends queued deliveries until ctx ends.
func (s *Server) webhookDeliveryLoop(ctx context.Context) {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	snd := &webhookSender{busy: map[string]bool{}}
	t := time.NewTicker(webhookPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A full claim means more is waiting, so claim again without the pause.
			for s.startDueWebhooks(ctx, time.Now(), rnd, snd) {
			}
		}
	}
}

// deliverDueWebhooks sends one round of due deliveries and waits for them.
func (s *Server) deliverDueWebhooks(ctx context.Context, now time.Time, rnd *rand.Rand) {
	snd := &webhookSender{busy: map[string]bool{}}
	s.startDueWebhooks(ctx, now, rnd, snd)
	snd.wg.Wait()
}

// startDueWebhooks claims due deliveries for hooks that are not already being
// sent to and starts one worker per hook. It reports whether the claim was full.
func (s *Server) startDueWebhooks(ctx context.Context, now time.Time, rnd *rand.Rand, snd *webhookSender) bool {
	snd.mu.Lock()
	busy := slices.Collect(maps.Keys(snd.busy))
	snd.mu.Unlock()
	limit := min(webhookClaimBatch, webhookWorkers-len(busy))
	if limit <= 0 {
		return false
	}
	due, err := s.st.ClaimDueDeliveries(ctx, now, webhookLease, limit, busy)
	if err != nil && ctx.Err() == nil {
		log.Printf("[webhook] claiming deliveries: %v", err)
	}
	var hookIDs []string
	perHook := map[string][]store.AgentWebhookDelivery{}
	for _, d := range due {
		if _, ok := perHook[d.WebhookID]; !ok {
			hookIDs = append(hookIDs, d.WebhookID)
		}
		perHook[d.WebhookID] = append(perHook[d.WebhookID], d)
	}
	for _, hookID := range hookIDs {
		// rand.Rand is not safe for concurrent use, so each worker gets its own.
		workerRnd := rnd
		if rnd != nil {
			workerRnd = rand.New(rand.NewSource(rnd.Int63()))
		}
		snd.mu.Lock()
		snd.busy[hookID] = true
		snd.mu.Unlock()
		snd.wg.Add(1)
		go func() {
			defer snd.wg.Done()
			defer func() {
				snd.mu.Lock()
				delete(snd.busy, hookID)
				snd.mu.Unlock()
			}()
			for _, d := range perHook[hookID] {
				status, err := s.attemptSandboxWebhook(ctx, d)
				if err := s.recordWebhookAttempt(ctx, d, status, err, workerRnd); err != nil {
					log.Printf("[webhook] %s: recording the attempt: %v", d.ID, err)
				}
			}
		}()
	}
	return len(due) == limit
}

// recordWebhookAttempt stores an attempt's outcome and schedules the retry, if any.
func (s *Server) recordWebhookAttempt(ctx context.Context, d store.AgentWebhookDelivery, status int, sendErr error, rnd *rand.Rand) error {
	if sendErr == nil && status >= 200 && status < 300 {
		if err := s.st.RecordDeliveryAttempt(ctx, d.ID, store.DeliverySent, status, "", time.Time{}); err != nil {
			return err
		}
		_, err := s.st.RecordWebhookResult(ctx, d.WebhookID, status, false, webhook.AutoDisableAfter)
		return err
	}

	reason := "the endpoint answered " + http.StatusText(status)
	if sendErr != nil {
		reason = sendErr.Error()
	}
	attempt := d.Attempts + 1
	gone := errors.Is(sendErr, errWebhookUndeliverable)
	if !gone && !webhook.Terminal(status, attempt) {
		return s.st.RecordDeliveryAttempt(ctx, d.ID, store.DeliveryPending, status, reason,
			time.Now().Add(webhook.Backoff(attempt, rnd)))
	}
	if err := s.st.RecordDeliveryAttempt(ctx, d.ID, store.DeliveryFailed, status, reason, time.Time{}); err != nil {
		return err
	}
	if gone {
		return nil
	}
	_, err := s.st.RecordWebhookResult(ctx, d.WebhookID, status, true, webhook.AutoDisableAfter)
	return err
}

// postSignedWebhook sends one signed body to a customer URL through the SSRF guard.
func postSignedWebhook(ctx context.Context, rawURL, secret, deliveryID string, body []byte) (int, error) {
	u, ips, err := webhook.ResolveAndPin(rawURL)
	if err != nil {
		return 0, err
	}
	ts := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SignatureHeader, webhook.Sign(secret, ts, body))
	req.Header.Set(webhook.TimestampHeader, strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set(webhook.DeliveryIDHeader, deliveryID)

	resp, err := webhook.PinnedClient(ips, webhook.DeliveryTimeout).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
