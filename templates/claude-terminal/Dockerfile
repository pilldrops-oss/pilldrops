FROM node:20-bookworm-slim

# Claude Code CLI in a browser terminal: ttyd serves a shell on the preview
# port — open the sandbox URL, type `claude`, done. No client install.
# ttyd comes from its static release binary (not in this image's apt repos).
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates procps \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL -o /usr/local/bin/ttyd \
       https://github.com/tsl0922/ttyd/releases/download/1.7.7/ttyd.x86_64 \
    && chmod +x /usr/local/bin/ttyd

RUN npm install -g @anthropic-ai/claude-code

ENV HOME=/root
WORKDIR /root

# start.sh (auto-started by init) runs ttyd on $PORT.

# The envd binary and overlay-init are injected by build-template.sh
