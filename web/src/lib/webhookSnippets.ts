import { shellQuote } from './launchKinds'

export const WEBHOOK_SNIPPETS = [
  { key: 'curl', label: 'curl' },
  { key: 'node', label: 'Node receiver' },
  { key: 'python', label: 'Python receiver' },
  { key: 'request', label: 'What we send' },
] as const

export type WebhookSnippetKey = typeof WEBHOOK_SNIPPETS[number]['key']

const NODE_RECEIVER = `import { createServer } from 'node:http'
import { createHmac, timingSafeEqual } from 'node:crypto'

const SECRET = process.env.WEBHOOK_SECRET

createServer((req, res) => {
  let body = ''
  req.on('data', c => (body += c))
  req.on('end', () => {
    const ts = String(req.headers['x-pilldrops-timestamp'])
    const want = 'sha256=' + createHmac('sha256', SECRET).update(ts + '.' + body).digest('hex')
    const got = String(req.headers['x-pilldrops-signature'])
    const fresh = Math.abs(Date.now() / 1000 - Number(ts)) <= 300
    const ok = fresh && got.length === want.length && timingSafeEqual(Buffer.from(got), Buffer.from(want))
    if (!ok) return res.writeHead(401).end()
    console.log(JSON.parse(body)) // post to a channel, update a ticket, ...
    res.writeHead(200).end()
  })
}).listen(8080)`

const PYTHON_RECEIVER = `import hashlib, hmac, json, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET = os.environ["WEBHOOK_SECRET"].encode()

class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        ts = self.headers.get("X-Pilldrops-Timestamp", "0")
        want = "sha256=" + hmac.new(SECRET, ts.encode() + b"." + body, hashlib.sha256).hexdigest()
        got = self.headers.get("X-Pilldrops-Signature", "")
        fresh = ts.isdigit() and abs(time.time() - int(ts)) <= 300
        if not (fresh and hmac.compare_digest(want, got)):
            self.send_response(401); self.end_headers(); return
        print(json.loads(body))  # post to a channel, update a ticket, ...
        self.send_response(200); self.end_headers()

HTTPServer(("", 8080), Hook).serve_forever()`

const SANDBOX_EVENT_REQUEST = `POST <your URL>
Content-Type: application/json
X-Pilldrops-Signature: sha256=<hex HMAC-SHA256 of timestamp + "." + body>
X-Pilldrops-Timestamp: <unix seconds>
X-Pilldrops-Delivery: <unique id, use it to drop repeats>

{
  "id": "whd_5f0c2a9b1d3e4f607182",
  "type": "sandbox.ended",
  "at": "2026-01-01T12:00:00Z",
  "sandbox": { "id": "sb_1a2b3c", "template": "python" },
  "data": {
    "reason": "destroyed",
    "started_at": "2026-01-01T11:45:00Z",
    "ran_seconds": 900,
    "vcpu": 2,
    "memory_mb": 1024
  }
}`

/** webhookSnippets fills the copy-paste examples; hookID is a placeholder until a webhook exists. */
export function webhookSnippets(o: {
  origin: string
  url: string
  events: string[]
  hookID?: string
}): Record<WebhookSnippetKey, string> {
  const base = `${o.origin}/v1/webhooks`
  const auth = `-H "Authorization: Bearer $PILLDROPS_API_KEY"`
  const hook = o.hookID ?? '<webhook id>'
  return {
    curl: [
      '# Make a key on the API Keys page first.',
      'export PILLDROPS_API_KEY=<your key>',
      '',
      '# Add the webhook. The reply holds the signing secret, once.',
      `curl -X POST ${shellQuote(base)} \\`,
      `  ${auth} \\`,
      `  -H 'Content-Type: application/json' \\`,
      `  -d ${shellQuote(JSON.stringify({ url: o.url, events: o.events }))}`,
      '',
      '# Send a test delivery to it.',
      `curl -X POST ${shellQuote(`${base}/${hook}/test`)} \\`,
      `  ${auth}`,
      '',
      '# List webhooks with their last delivery.',
      `curl ${shellQuote(base)} \\`,
      `  ${auth}`,
    ].join('\n'),
    node: NODE_RECEIVER,
    python: PYTHON_RECEIVER,
    request: SANDBOX_EVENT_REQUEST,
  }
}
