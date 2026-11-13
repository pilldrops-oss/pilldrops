package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/webhook"
)

const (
	webhookPollInterval = 2 * time.Second
	// webhookClaimBatch deliveries are sent one after another, so the lease must
	// outlast a whole batch of slow receivers (DNS plus delivery timeout each).
	webhookClaimBatch = 10
	webhookLease      = 2 * time.Minute
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

// webhookDeliveryLoop sends queued deliveries until ctx ends.
// ponytail: one sequential sender per node; add workers if delivery volume outgrows it.
func (s *Server) webhookDeliveryLoop(ctx context.Context) {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	t := time.NewTicker(webhookPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.deliverDueWebhooks(ctx, time.Now(), rnd)
		}
	}
}

func (s *Server) deliverDueWebhooks(ctx context.Context, now time.Time, rnd *rand.Rand) {
	due, err := s.st.ClaimDueDeliveries(ctx, now, webhookLease, webhookClaimBatch)
	if err != nil && ctx.Err() == nil {
		log.Printf("[webhook] claiming deliveries: %v", err)
	}
	for _, d := range due {
		status, err := s.attemptSandboxWebhook(ctx, d)
		if err := s.recordWebhookAttempt(ctx, d, status, err, rnd); err != nil {
			log.Printf("[webhook] %s: recording the attempt: %v", d.ID, err)
		}
	}
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
