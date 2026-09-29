"""Sample web app for sandboxd — a tiny notes API that honors $PORT.

Deploy it with:  ./cli/pilldrops deploy examples/sample-webapp --start "python3 app.py" --port 8000
"""
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

NOTES = ["welcome to sandboxd", "deployed via the CLI"]

# Note: use __ITEMS__ + .replace() (not .format) so the CSS braces below stay literal.
PAGE = """<!doctype html><html><head><title>sandboxd sample</title>
<style>body{font-family:system-ui;max-width:640px;margin:40px auto;padding:0 16px}
li{padding:6px 0;border-bottom:1px solid #eee}code{background:#f4f4f4;padding:2px 6px;border-radius:4px}</style>
</head><body>
<h1 id=title>sandboxd notes</h1>
<p>Served from an isolated sandbox. API at <code>/api/notes</code>.</p>
<ul id=notes>__ITEMS__</ul>
</body></html>"""


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, body, ctype="text/html"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.end_headers()
        self.wfile.write(body.encode())

    def do_GET(self):
        if self.path == "/api/notes":
            self._send(200, json.dumps({"notes": NOTES, "count": len(NOTES)}), "application/json")
        elif self.path == "/health":
            self._send(200, json.dumps({"status": "ok"}), "application/json")
        else:
            items = "".join(f"<li>{n}</li>" for n in NOTES)
            self._send(200, PAGE.replace("__ITEMS__", items))

    def log_message(self, *a):  # quiet
        pass


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8000"))
    print(f"sample-webapp listening on $PORT={port}")
    HTTPServer(("0.0.0.0", port), Handler).serve_forever()
