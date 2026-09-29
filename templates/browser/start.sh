#!/bin/sh
# Launch headless Chromium with CDP, then run the screencast bridge on $PORT.
PORT="${PORT:-8000}"
CHROME=$(ls /ms-playwright/chromium-*/chrome-linux*/chrome 2>/dev/null | head -1)
[ -z "$CHROME" ] && CHROME=$(command -v chromium 2>/dev/null || command -v chromium-browser 2>/dev/null || command -v google-chrome 2>/dev/null)
"$CHROME" --headless=new --no-sandbox --disable-dev-shm-usage --disable-gpu \
  --remote-debugging-port=9222 --remote-debugging-address=127.0.0.1 \
  --remote-allow-origins=* \
  --window-size=1280,800 about:blank >/tmp/chrome.log 2>&1 &
exec node /app/bridge.js
