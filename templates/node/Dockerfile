FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# typescript 5: ts-node cannot load 7, and from 6 on tsc no longer picks up
# @types by itself. tsc and ts-node look for @types only in node_modules above
# the project, never in the global prefix, so the global one is linked at the root.
RUN npm install -g typescript@5 tsx ts-node @types/node@20 \
    && mkdir /node_modules \
    && ln -s "$(npm root -g)/@types" /node_modules/@types

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh
