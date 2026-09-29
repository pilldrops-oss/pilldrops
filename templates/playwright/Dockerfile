FROM node:20-bookworm-slim

# Browsers live at a fixed path (not $HOME) so every exec'd process finds them
# regardless of its environment. Baked into /etc/sandbox-env by the builder.
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

WORKDIR /root

# Baked project: config records everything by default; example suite included.
# Users drop their own specs into /root/tests and run `pwtest` — nothing to
# install or configure.
COPY package.json playwright.config.js /root/
COPY tests /root/tests
COPY pwrecord.js /root/.pwrecord.js

RUN npm install --omit=dev \
    && npm install -g serve \
    && npx playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/* /root/.npm

# CLI helpers: `pwtest [args]` runs the suite, `pwrecord <url> [secs]` records
# a headless browsing session. Both write under /root/recordings, which
# start.sh serves on the preview port.
COPY pwtest pwrecord /usr/local/bin/
RUN chmod +x /usr/local/bin/pwtest /usr/local/bin/pwrecord

# start.sh (auto-started by init) serves /root/recordings on $PORT (8080).

# The envd binary and overlay-init are injected by build-template.sh
