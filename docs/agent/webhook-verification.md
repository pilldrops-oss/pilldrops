# Verifying agent webhooks

Every delivery carries three headers:

| Header | Contents |
|---|---|
| `X-Pilldrops-Signature` | `sha256=<hex>` — HMAC-SHA256 over `timestamp + "." + body` |
| `X-Pilldrops-Timestamp` | Unix seconds, the same value that was signed |
| `X-Pilldrops-Delivery` | A stable id. **Retries reuse it** |

## The two mistakes that make verification useless

**Comparing signatures with `==`.** String equality short-circuits on the first differing byte, so
how long the comparison takes tells an attacker how much of their guess was right. Use your
language's constant-time compare — `crypto.timingSafeEqual`, `hmac.compare_digest`.

**Ignoring the timestamp.** This is the one people skip, and skipping it throws away most of what
the signature buys you. The signature covers the timestamp, so if you do not check that the
timestamp is *recent*, a captured request stays valid forever and anyone who saw one delivery can
replay it whenever they like. Reject anything more than **5 minutes** from your clock, in
**either** direction — a timestamp far in the future is as suspicious as a stale one, and
checking only the past leaves a forger free to grant themselves unlimited validity.

Even with the check, replay is *bounded*, not eliminated: a delivery captured and resent inside
the window verifies, and it has to — otherwise our honest retries would fail. Eliminating replay
is your side of the contract, and it is why retries reuse the delivery id: **dedupe on
`X-Pilldrops-Delivery`** and at-least-once delivery becomes safe to act on.

## Node

```js
import crypto from "node:crypto";

const MAX_SKEW_SECONDS = 300; // 5 minutes, both directions

/** Returns true only if this request is genuinely ours and genuinely fresh. */
export function verify(req, rawBody, secret) {
  const signature = req.headers["x-pilldrops-signature"];
  const timestamp = req.headers["x-pilldrops-timestamp"];
  if (!signature || !timestamp) return false;

  // Freshness FIRST: it is the cheap check, and it is the one that stops a replay.
  const age = Math.abs(Math.floor(Date.now() / 1000) - Number(timestamp));
  if (!Number.isFinite(age) || age > MAX_SKEW_SECONDS) return false;

  // The separator matters. Without it, (ts="1", body="23") and (ts="12", body="3") sign
  // identically, so a different timestamp could be fed to you under the same signature.
  const expected =
    "sha256=" +
    crypto.createHmac("sha256", secret).update(`${timestamp}.`).update(rawBody).digest("hex");

  const a = Buffer.from(expected);
  const b = Buffer.from(signature);
  // Length check before timingSafeEqual, which throws on a mismatch rather than returning
  // false — and a thrown exception in a webhook handler is a 500, which we then retry.
  if (a.length !== b.length) return false;
  return crypto.timingSafeEqual(a, b);
}
```

**Use the raw body.** Verify the exact bytes we sent, before any JSON parsing. `JSON.parse`
followed by `JSON.stringify` reorders keys and changes whitespace, and the signature will not
match. In Express that means `express.raw({ type: "application/json" })` on this route, not
`express.json()`.

```js
app.post("/hooks/agent", express.raw({ type: "application/json" }), (req, res) => {
  if (!verify(req, req.body, process.env.PILLDROPS_WEBHOOK_SECRET)) {
    return res.sendStatus(401);
  }
  const id = req.headers["x-pilldrops-delivery"];
  if (alreadyHandled(id)) return res.sendStatus(200); // a retry, not a new event
  markHandled(id);

  handle(JSON.parse(req.body.toString("utf8")));
  res.sendStatus(200);
});
```

## Python

```python
import hashlib
import hmac
import time

MAX_SKEW_SECONDS = 300  # 5 minutes, both directions


def verify(headers, raw_body: bytes, secret: str) -> bool:
    """True only if this request is genuinely ours and genuinely fresh."""
    signature = headers.get("X-Pilldrops-Signature")
    timestamp = headers.get("X-Pilldrops-Timestamp")
    if not signature or not timestamp:
        return False

    try:
        age = abs(int(time.time()) - int(timestamp))
    except ValueError:
        return False
    # Freshness first: without it the signature stays valid forever and any captured
    # delivery can be replayed at will.
    if age > MAX_SKEW_SECONDS:
        return False

    mac = hmac.new(secret.encode(), digestmod=hashlib.sha256)
    mac.update(timestamp.encode())
    mac.update(b".")          # the separator is part of the signed material
    mac.update(raw_body)
    expected = "sha256=" + mac.hexdigest()

    # compare_digest, never ==: string equality leaks how much of a guess was correct
    # through how long it took to reject it.
    return hmac.compare_digest(expected, signature)
```

```python
from flask import Flask, request, abort

app = Flask(__name__)
seen = set()  # use something durable in production


@app.post("/hooks/agent")
def agent_hook():
    # request.get_data() is the raw bytes. request.json would reparse and reserialise,
    # which changes key order and whitespace and breaks the signature.
    body = request.get_data()
    if not verify(request.headers, body, WEBHOOK_SECRET):
        abort(401)

    delivery = request.headers["X-Pilldrops-Delivery"]
    if delivery in seen:
        return "", 200  # a retry of something already handled
    seen.add(delivery)

    handle(request.get_json())
    return "", 200
```

## Testing your endpoint

`POST /v1/sandboxes/{id}/agent/webhooks/{hookID}/test` sends one signed delivery and reports what
your endpoint answered. Limited to 5 per minute per hook — the endpoint makes our server call an
address you chose, so it needs a budget.

Worth checking deliberately, because these are the failure modes that only appear in production:

1. **A stale timestamp is rejected.** Sign a body with a timestamp ten minutes old and confirm
   your handler refuses it. If it accepts, your timestamp check is not wired up, and you will not
   find out from a passing test suite.
2. **A tampered body is rejected.** Change one byte after signing.
3. **A repeated delivery id is handled once.** Send the same delivery twice and confirm the
   side effect happened once.

## What we retry, and what we give up on

- Up to **6 attempts**, with full-jitter exponential backoff.
- A `4xx` other than `408` or `429` **stops immediately** — you understood the request and
  refused it, and repeating it just delivers the same refusal five more times.
- **20 consecutive** failures disables the hook, with the reason recorded. Consecutive, not
  cumulative: 20 failures spread over a year is a flaky receiver, 20 in a row is an endpoint that
  has gone away.
- Deliveries that never got through are readable at
  `GET /v1/sandboxes/{id}/agent/webhooks/failures`, so a silent hook is diagnosable rather than
  just quiet.

Answer `2xx` as soon as you have *stored* the event. Doing the work first means a slow handler
turns into our retries, and then into duplicate deliveries you have to dedupe anyway.
