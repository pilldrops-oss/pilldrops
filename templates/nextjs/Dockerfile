FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g create-next-app typescript tailwindcss

# Pre-baked starter app: `cd /root/app && npm run dev` serves immediately —
# no scaffold step, no install wait, no 502 while npm downloads the world.
# The dev/start scripts bind $PORT (injected by the platform) on 0.0.0.0.
WORKDIR /root/app
COPY app/package.json app/next.config.js ./
COPY app/pages ./pages
RUN npm install --no-audit --no-fund && npm cache clean --force

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh
