package webhook

import (
	"errors"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReplayIsRejectedBecauseTheTimestampIsSigned(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{"event":"turn.completed"}`)
	signedAt := time.Now()
	sig := Sign(secret, signedAt, body)

	if err := Verify(secret, sig, signedAt.Unix(), body, signedAt.Add(time.Second)); err != nil {
		t.Fatalf("a fresh delivery was rejected: %v", err)
	}

	err := Verify(secret, sig, signedAt.Unix(), body, signedAt.Add(time.Hour))
	if err == nil {
		t.Fatal("an hour-old replay verified — a captured delivery would be resendable forever")
	}
	if !strings.Contains(err.Error(), "window") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}

	// Must differ from signedAt by at least a second or it is the same signed material.
	replayTS := signedAt.Add(30 * time.Second)
	if err := Verify(secret, sig, replayTS.Unix(), body, replayTS); err == nil {
		t.Fatal("a re-timestamped replay verified — the timestamp is not being signed")
	}
}

// A replay inside the skew window verifies by design; deduping on the delivery id is the receiver's job.
func TestTimestampBindingBoundsReplayItDoesNotEliminateIt(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{"event":"turn.completed"}`)
	at := time.Now()
	sig := Sign(secret, at, body)

	if err := Verify(secret, sig, at.Unix(), body, at.Add(time.Minute)); err != nil {
		t.Fatalf("a delivery one minute old was rejected, which would break honest retries: %v", err)
	}
	if err := Verify(secret, sig, at.Unix(), body, at.Add(MaxSkew+time.Second)); err == nil {
		t.Fatal("a delivery outside the skew window verified")
	}
}

func TestFutureTimestampsAreAlsoRejected(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{}`)
	future := time.Now().Add(2 * time.Hour)
	sig := Sign(secret, future, body)

	if err := Verify(secret, sig, future.Unix(), body, time.Now()); err == nil {
		t.Fatal("a delivery dated two hours in the future verified")
	}
}

func TestSeparatorPreventsFieldConfusion(t *testing.T) {
	const secret = "s"
	a := Sign(secret, time.Unix(1, 0), []byte("23"))
	b := Sign(secret, time.Unix(12, 0), []byte("3"))
	if a == b {
		t.Fatal("two different (timestamp, body) pairs produced the same signature")
	}
}

func TestVerifyRejectsAWrongSecret(t *testing.T) {
	body := []byte(`{"x":1}`)
	now := time.Now()
	sig := Sign("right", now, body)
	if err := Verify("wrong", sig, now.Unix(), body, now); err == nil {
		t.Fatal("a signature verified against the wrong secret")
	}
	if err := Verify("right", sig, now.Unix(), []byte(`{"x":2}`), now); err == nil {
		t.Fatal("a tampered body verified")
	}
}

func TestBlockedTargetsAreRefused(t *testing.T) {
	for name, raw := range map[string]string{
		"cloud metadata":     "https://169.254.169.254/latest/meta-data/",
		"loopback":           "https://127.0.0.1/hook",
		"loopback by name":   "https://localhost/hook",
		"rfc1918 ten":        "https://10.0.0.5/hook",
		"rfc1918 one-ninety": "https://192.168.1.1/hook",
		"rfc1918 seventeen":  "https://172.16.0.1/hook",
		"cgnat":              "https://100.64.0.1/hook",
		"unspecified":        "https://0.0.0.0/hook",
		"ipv6 loopback":      "https://[::1]/hook",
		"ipv6 link local":    "https://[fe80::1]/hook",
		// The signature proves origin, not confidentiality.
		"plaintext": "http://example.com/hook",
		"no host":   "https:///hook",
		"garbage":   "https://%zz",
	} {
		if _, _, err := ResolveAndPin(raw); err == nil {
			t.Errorf("%s (%s) was allowed", name, raw)
		} else if !errors.Is(err, ErrBlockedTarget) {
			t.Errorf("%s: error is not ErrBlockedTarget: %v", name, err)
		}
	}
}

func TestAnyPrivateAddressDisqualifiesTheHost(t *testing.T) {
	// Exercised through blockedIP; forcing a mixed DNS answer would mean owning a resolver.
	mixed := []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("169.254.169.254")}
	anyBlocked := false
	for _, ip := range mixed {
		if blockedIP(ip) {
			anyBlocked = true
		}
	}
	if !anyBlocked {
		t.Fatal("a metadata address was not recognised as blocked")
	}
}

func TestPinnedClientOnlyDialsPinnedAddresses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dialed := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dialed <- c.LocalAddr().String()
			c.Close()
		}
	}()

	// Pinned to loopback with an unrelated hostname; the dialer refuses loopback, which
	// is the production behaviour this asserts.
	c := PinnedClient([]net.IP{net.ParseIP("127.0.0.1")}, 2*time.Second)
	req, _ := http.NewRequest("GET", "https://example.invalid:"+strconv.Itoa(port)+"/hook", nil)
	_, err = c.Do(req)
	if err == nil {
		t.Fatal("the pinned client dialed a loopback address")
	}
	if !strings.Contains(err.Error(), ErrBlockedTarget.Error()) {
		t.Errorf("the refusal is not the SSRF guard: %v", err)
	}
}

func TestRedirectsAreRefusedOutright(t *testing.T) {
	c := PinnedClient([]net.IP{net.ParseIP("93.184.216.34")}, time.Second)
	if c.CheckRedirect == nil {
		t.Fatal("redirects are followed blindly")
	}
	err := c.CheckRedirect(&http.Request{}, nil)
	if err == nil {
		t.Fatal("a redirect was permitted")
	}
	if !errors.Is(err, ErrBlockedTarget) {
		t.Errorf("redirect refusal is not the SSRF guard: %v", err)
	}
}

func TestNoProxyIsUsed(t *testing.T) {
	c := PinnedClient([]net.IP{net.ParseIP("93.184.216.34")}, time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("no transport to inspect")
	}
	if tr.Proxy != nil {
		t.Fatal("the pinned client honours a proxy, which can redirect it off the pinned address")
	}
}

func TestTurnOutputIsNotDeliverable(t *testing.T) {
	if EventEligible("turn.output") {
		t.Fatal("turn.output is deliverable — this is a denial-of-service we would be building " +
			"for the customer, on their own endpoint")
	}
	r := IneligibleReason("turn.output")
	if !strings.Contains(r, "flood") || !strings.Contains(r, "turn.completed") {
		t.Errorf("the refusal does not say why or what to do instead: %q", r)
	}
}

func TestEligibilityIsAnAllowlist(t *testing.T) {
	for _, want := range []string{"turn.completed", "approval.requested", "spend.threshold"} {
		if !EventEligible(want) {
			t.Errorf("%s should be deliverable", want)
		}
	}
	for _, unknown := range []string{"turn.output", "resource.sample", "some.future.firehose", ""} {
		if EventEligible(unknown) {
			t.Errorf("%q is deliverable without having been considered", unknown)
		}
	}
}

func TestFourXXIsTerminalExceptTheTwoThatAreNotRefusals(t *testing.T) {
	for _, s := range []int{400, 401, 403, 404, 410, 418, 422} {
		if Retryable(s) {
			t.Errorf("%d is retried — a permanent rejection would be hammered", s)
		}
		if !Terminal(s, 1) {
			t.Errorf("%d does not stop the delivery on the first attempt", s)
		}
	}
	for _, s := range []int{408, 429} {
		if !Retryable(s) {
			t.Errorf("%d is treated as permanent, but it asks to be retried", s)
		}
	}
	for _, s := range []int{500, 502, 503, 504, 0} {
		if !Retryable(s) {
			t.Errorf("%d is treated as permanent", s)
		}
	}
}

func TestAttemptsAreBounded(t *testing.T) {
	if Terminal(500, MaxAttempts-1) {
		t.Error("gave up before the attempt budget was spent")
	}
	if !Terminal(500, MaxAttempts) {
		t.Error("a 5xx retries past the attempt budget")
	}
}

func TestBackoffIsJittered(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	seen := map[time.Duration]bool{}
	for range 20 {
		seen[Backoff(4, rnd)] = true
	}
	if len(seen) < 5 {
		t.Fatalf("backoff produced only %d distinct delays across 20 calls — every pending "+
			"delivery would retry in lockstep", len(seen))
	}
	for a := 1; a <= 12; a++ {
		if d := Backoff(a, rnd); d <= 0 || d > maxBackoff {
			t.Fatalf("Backoff(%d) = %v, outside (0, %v]", a, d, maxBackoff)
		}
	}
	if Backoff(3, nil) != 4*time.Second {
		t.Errorf("Backoff(3, nil) = %v, want the plain 4s schedule", Backoff(3, nil))
	}
	if d := Backoff(0, nil); d != baseBackoff {
		t.Errorf("Backoff(0) = %v, want the base delay", d)
	}
}

func TestAutoDisableCountsConsecutiveFailures(t *testing.T) {
	if AutoDisableAfter <= MaxAttempts {
		t.Fatalf("AutoDisableAfter (%d) is not above MaxAttempts (%d), so a single failing "+
			"delivery would disable the hook", AutoDisableAfter, MaxAttempts)
	}
}
