# Lightweight "computer-use" browser: headless Chromium + a tiny CDP screencast
# bridge. No X server, no VNC, no desktop environment — just Chromium and Node.
# The bridge streams JPEG frames over a WebSocket and relays mouse/keyboard input
# as CDP events, so the preview URL is a live, controllable browser.
FROM node:20-bookworm-slim

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY bridge.js /app/bridge.js

# Only Chromium (not the full Playwright browser set) + its OS deps, plus `ws`
# for the bridge. Kept lean — this is the whole point vs. the noVNC desktop.
RUN npm init -y >/dev/null 2>&1 \
    && npm install ws >/dev/null 2>&1 \
    && npx -y playwright@1.49.0 install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/* /root/.npm /root/.cache

# start.sh (auto-started by init) launches Chromium headless with CDP, then the
# bridge on $PORT. The envd binary and overlay-init are injected by build-template.sh.
COPY start.sh /usr/local/bin/start.sh
RUN chmod +x /usr/local/bin/start.sh
