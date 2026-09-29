# ─────────────────────────────────────────────────────────────
#  Sandbox Platform — Makefile
# ─────────────────────────────────────────────────────────────
#
#  Dev:              make dev          → real execution on :8080
#  Prod:             make prod-deploy HOST=x.x.x.x  → build + deploy + restart
#
# ─────────────────────────────────────────────────────────────

SHELL := /bin/bash
HOST  ?= ""

# ─── Dev Mode ────────────────────────────────────────────────
# Real envd execution. No Firecracker, no DB. Works on macOS/Linux.
# Dashboard: http://localhost:8080/dashboard
# API:       http://localhost:8080/v1/sandboxes

.PHONY: dev dev-build sandboxd orchestrator setplan run serve

sandboxd:
	@go build -o bin/sandboxd ./cmd/sandboxd/
	@echo "── Built bin/sandboxd"

# The control plane: scheduler, proxy across nodes, and the dashboard
# (ORCHESTRATOR_UI=off turns the dashboard off).
orchestrator:
	@go build -o bin/orchestrator ./cmd/orchestrator/
	@echo "── Built bin/orchestrator"

# Assigns a limit preset to an org. An org with none is unlimited, so this is
# only needed to bound tenants on a shared deployment.
setplan:
	@go build -o bin/setplan ./cmd/setplan/
	@echo "── Built bin/setplan"

run: sandboxd
	@SANDBOXD_DATA=$${SANDBOXD_DATA:-/tmp/sandboxd} PORT=$${PORT:-8080} ./bin/sandboxd

# All-in-one: rebuild + re-embed the web UI, build sandboxd, then run it.
# Override the port with PORT=8090 (default 8080).
serve: web sandboxd
	@echo "── Serving sandboxd (fresh web UI) on http://localhost:$${PORT:-8080}"
	@SANDBOXD_DATA=$${SANDBOXD_DATA:-/tmp/sandboxd} PORT=$${PORT:-8080} ./bin/sandboxd

dev-build:
	@echo "── Building envd (local)..."
	@go build -o bin/envd-local ./cmd/envd/
	@echo "── Building devserver..."
	@go build -o bin/devserver ./cmd/devserver/
	@echo "── Ready"

# PORTAL_LOCAL_RUNTIME opts this process into the in-process sandbox runtime,
# which shells out on YOUR machine with no isolation. That is what you want on a
# laptop with no fleet attached, and it is set here rather than defaulted so a
# deployed portal missing its orchestrator credentials refuses instead of
# quietly becoming a shell server. Never set it in production.
dev: dev-build
	@PORTAL_LOCAL_RUNTIME=1 ./bin/devserver

# ─── Production Build ────────────────────────────────────────
# Cross-compiles for Linux. Requires Go installed locally.

.PHONY: build sandboxd-linux sandboxd-core envd agentd agentd-local proto

proto:
	protoc --go_out=. --go-grpc_out=. proto/envd.proto
	@if [ -d github.com ]; then \
		mv github.com/pilldrops-oss/pilldrops/proto/envdpb/*.go proto/envdpb/; \
		rm -rf github.com; \
	fi

envd:
	@echo "── Building envd (static, linux/amd64)..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/envd ./cmd/envd/
	@echo "── Built: bin/envd ($$(du -h bin/envd | cut -f1))"

# agentd is the in-sandbox agent supervisor. Built for the GUEST (linux/amd64) and
# baked into agent templates by scripts/build-template.sh; the docker runtime
# bind-mounts the same binary. Non-agent templates never receive it.
agentd:
	@echo "── Building agentd (static, linux/amd64)..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/agentd ./cmd/agentd/
	@echo "── Built: bin/agentd ($$(du -h bin/agentd | cut -f1))"

# The customer CLI, built static for the GUEST from the sibling repo and baked
# into every template (templatebuild, scripts/build-template.sh) or bind-mounted
# by the docker runtime; the node reads it from PD_CLI_PATH
# (default /opt/pilldrops/bin/pilldrops-linux).
CLI_REPO ?= ../pilldrops-cli
cli-linux:
	@echo "── Building pilldrops CLI (static, linux/amd64) from $(CLI_REPO)..."
	@cd $(CLI_REPO) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(CURDIR)/bin/pilldrops-linux ./cmd/pilldrops
	@echo "── Built: bin/pilldrops-linux ($$(du -h bin/pilldrops-linux | cut -f1))"

# Host build, for driving a local docker-runtime sandbox: point the runtime at it
# with PD_AGENTD_PATH=bin/agentd-local.
agentd-local:
	@echo "── Building agentd (local host)..."
	@go build -o bin/agentd-local ./cmd/agentd/
	@echo "── Built: bin/agentd-local"

# No -tags, and specifically not `dev`: an untagged build contains no runtime
# that runs customer code without a boundary. See release.yml and
# internal/nodeboot/releaseclosure_test.go, which asserts it against the closure.
sandboxd-linux:
	@echo "── Building sandboxd (linux/amd64)..."
	@GOOS=linux GOARCH=amd64 go build -o bin/sandboxd-linux ./cmd/sandboxd/
	@GOOS=linux GOARCH=amd64 go build -o bin/pilldrops-join-linux ./cmd/pilldrops-join/
	@echo "── Built: bin/sandboxd-linux ($$(du -h bin/sandboxd-linux | cut -f1)), bin/pilldrops-join-linux"

# sandboxd-core is THE BOUNDARY, BUILT — not yet the deploy artifact.
#
# `-tags core` compiles out the agent surface, serverless functions, cron and
# the MCP integrations, along with the chat routes that depend on them. What
# comes out runs sandboxes and meters them, and answers 404 on every route
# belonging to a feature it does not have.
#
# Do not ship this expecting the full node API. It is built so
# scripts/tier-boundary-guard.sh has a tagged link closure to check: that is
# what turns "the agent is optional" from a claim in a document into a property
# of an artifact. Deliberately NOT wired into `build` or release.yml.
#
# Not to be confused with the pre-existing `test-core` target, which means the
# central packages and has nothing to do with this build tag.
sandboxd-core:
	@echo "── Building sandboxd-fleet with -tags core (boundary check, not a deploy artifact)..."
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags core -o bin/sandboxd-fleet-core ./cmd/sandboxd-fleet/
	@echo "── Built: bin/sandboxd-fleet-core ($$(du -h bin/sandboxd-fleet-core | cut -f1))"
	@./scripts/tier-boundary-guard.sh

build: envd agentd sandboxd-linux
	@echo "── All binaries ready in bin/"

# ─── Database ────────────────────────────────────────────────

.PHONY: db-up db-down db-migrate db-reset db-shell

db-up:
	@echo "── Starting PostgreSQL..."
	@docker compose up -d
	@echo "── Waiting for postgres to be ready..."
	@until docker exec $$(docker ps -qf "name=postgres") pg_isready -U sandbox 2>/dev/null; do sleep 1; done
	@echo "── PostgreSQL ready on localhost:5432"

db-down:
	@docker compose down

# There is no migrate step. The schema lives in internal/store/migrations.go and
# store.Open() applies it on every connect, so the server migrates itself at
# startup. This target used to psql in a hand-written 001_initial.sql that
# declared an INCOMPATIBLE users table (id UUID PK … vs owner_id TEXT PK …);
# because the real migrations are CREATE TABLE IF NOT EXISTS, they then skipped
# silently and every query on owner_id/last_seen/login_count failed.
db-migrate: db-up
	@echo "── Nothing to do: the server applies internal/store/migrations.go at startup."

db-reset: db-down
	@docker compose down -v
	@echo "── Database volumes removed"
	@$(MAKE) db-up db-migrate

db-shell:
	@PGPASSWORD=sandbox_dev psql -h localhost -U sandbox -d sandbox

# ─── Production Run (local Linux) ───────────────────────────
# For running the real server on a Linux host with Firecracker.

.PHONY: prod-run prod-setup

prod-setup:
	@echo "── Running host setup (requires root)..."
	sudo ./scripts/setup-host.sh
	@echo "── Building template..."
	sudo ./scripts/build-template.sh python-3.11
	@echo "── Host ready"

prod-run: sandboxd
	@echo ""
	@echo "  ┌──────────────────────────────────────────┐"
	@echo "  │  Production Mode — Real VMs (Firecracker) │"
	@echo "  │  API:       http://localhost:8080/v1     │"
	@echo "  │  Dashboard: http://localhost:8080/dashboard│"
	@echo "  │                                          │"
	@echo "  │  Press Ctrl+C to stop                    │"
	@echo "  └──────────────────────────────────────────┘"
	@echo ""
	./bin/sandboxd

# ─── Production Deploy (remote Linux host) ───────────────────

.PHONY: prod-deploy

# NOTE: depends on `web` so the embedded dashboard (internal/webui/dist) is
# rebuilt from web/src before sandboxd-linux compiles it in — otherwise a stale
# UI ships silently. Guards below refuse a dirty tree (deploys whatever is on the
# laptop) and a non-main branch (ships the wrong code — a real past incident);
# override the branch check with ALLOW_BRANCH=1 for an intentional hotfix.
prod-deploy: web sandboxd-linux envd
ifndef HOST
	$(error HOST is required. Usage: make prod-deploy HOST=your-server-ip)
endif
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "✗ refusing to deploy: working tree is dirty — commit or stash first:"; \
		git status --short; \
		exit 1; \
	fi
	@branch=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$branch" != "main" ] && [ "$(ALLOW_BRANCH)" != "1" ]; then \
		echo "✗ refusing to deploy from '$$branch' (not main). Re-run with ALLOW_BRANCH=1 to override."; \
		exit 1; \
	fi
	@echo "── Deploying sandboxd to $(HOST)..."
	rsync -avz --progress \
		--exclude='.git' \
		--exclude='*.env' \
		--exclude='.env' \
		--exclude='node_modules' \
		--exclude='screenshots' \
		--exclude='.playwright-mcp' \
		. root@$(HOST):/opt/pilldrops/
	@echo "── Uploading binaries..."
	ssh root@$(HOST) "mkdir -p /opt/pilldrops/bin"
	scp bin/sandboxd-linux bin/envd root@$(HOST):/opt/pilldrops/bin/
	@echo "── Installing the packaged systemd unit (canonical: install/install.sh)..."
	scp packaging/sandboxd.service root@$(HOST):/etc/systemd/system/sandboxd.service
	ssh root@$(HOST) "systemctl daemon-reload && systemctl enable sandboxd && systemctl restart sandboxd"
	@echo ""
	@echo "  ┌──────────────────────────────────────────┐"
	@echo "  │  Deployed to $(HOST)                     │"
	@echo "  │  API:       http://$(HOST):8080/v1       │"
	@echo "  │  Dashboard: http://$(HOST):8080/dashboard│"
	@echo "  │                                          │"
	@echo "  │  Check: ssh root@$(HOST) journalctl -u sandboxd -f│"
	@echo "  └──────────────────────────────────────────┘"

# ─── Templates ───────────────────────────────────────────────

.PHONY: template template-python template-node

template-python:
	./scripts/build-template.sh python-3.11

# Canonical dir is templates/node (the old node-20 target pointed at a
# directory that doesn't exist).
template-node:
	./scripts/build-template.sh node

template: template-python

# ─── Tests ───────────────────────────────────────────────────

.PHONY: check test test-api lint test-cover test-core build-all docs docs-check bench-latency security-suite security-scan security-scan-update

# Regenerate the agent docs from the code that implements them (§14).
#
# The reference comes from openapi.json, the capability matrix from the launch manifest, and
# the limits from the constants the server enforces — so none of them can drift without a test
# failing. Run this after changing any of those and commit the result.
docs:
	@echo "── Generating agent docs..."
	go run ./cmd/gendocs -out docs/agent

# Fails if the checked-in docs are stale. Suitable for CI: the same check runs as a Go test,
# but this gives a one-line answer without running the whole suite.
docs-check:
	@go run ./cmd/gendocs -out docs/agent -check

# Everything CI would run, including the parts that only mean something on
# Linux (they run in a container). A check that could not run reports SKIP and
# is listed again at the end — never silently as a pass. --strict makes a skip
# a failure.
#
# The Linux half runs on CHECK_HOST when set, and it should be: that box is
# native amd64 (what CI runs) with real KVM, so it can exercise Firecracker, the
# jailer and vsock. This machine cannot. Export BENCH_HOST from your own notes;
# the address is never committed.
#
#   make check CHECK_HOST=$$BENCH_HOST   # the real thing
#   make check                           # local containers, no KVM — says so
#   make check STRICT=1                  # a skip becomes a failure
#   make check ONLY=guard                # substring filter, for iterating
check:
	@CHECK_HOST="$(CHECK_HOST)" ./scripts/check.sh $(if $(STRICT),--strict,) $(if $(ONLY),--only=$(ONLY),)

test:
	@echo "── Running Go tests..."
	go test -race ./...
	@echo "── Checking no package with tests silently ran none..."
	@./scripts/no-silent-skips.sh || (echo "" && \
	  echo "The suite above reported success without running those tests." && exit 1)

test-cover:
	@echo "── Running Go tests with coverage..."
	go test -race -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1
	@echo "── Coverage report: coverage.out"

test-core:
	@echo "── Testing sandboxd core packages (macOS/Linux)..."
	go test -race ./internal/runtime/... ./internal/store/... ./internal/sandbox/daemon/... ./internal/proxy/...

test-api:
	@echo "── Running full API test suite against localhost:8080..."
	@./examples/test_api.sh

# ─── Benchmarks ──────────────────────────────────────────────
#
# A latency claim about this platform is either a file in docs/benchmarks/ or
# it is folklore. This target produces one.
#
# Run it ON the node under test, not across a WAN: the measurement includes
# every hop, so a remote run measures your internet connection as well as the
# platform, and the two are not separable afterwards. Running on the node is
# also what lets the result record the node's real CPU and kernel rather than
# leaving those fields empty.
#
#   RUNTIME       required — the node's runtime. Not discoverable through the
#                 API until GET /v1/capabilities exists, so it is recorded as
#                 an operator assertion (runtime_source: "flag").
#   RUNNER_CLASS  required — the machine class (e.g. kvm-bare-1). Results are
#                 only comparable within a class.
#   CACHE         hot (default) | warm | cold. "cold" refuses until there is an
#                 artifact store to re-fetch a template from.
#   BENCH_HOST    optional API base URL. Named BENCH_HOST, not HOST, because
#                 this Makefile already defines HOST ?= "" for the deploy
#                 targets — reusing it would pass an empty -host on every run.
#
# Example, on the reference host:
#   PILLDROPS_API_KEY=$$(sudo cat /var/lib/sandbox/state/bootstrap.key) \
#     make bench-latency RUNTIME=firecracker RUNNER_CLASS=kvm-bare-1 TEMPLATE=base N=30
bench-latency:
	@test -n "$(RUNTIME)" || (echo "RUNTIME= is required (firecracker|docker|gvisor)" && exit 2)
	@test -n "$(RUNNER_CLASS)" || (echo "RUNNER_CLASS= is required (e.g. kvm-bare-1)" && exit 2)
	@echo "── Latency benchmark: runtime=$(RUNTIME) template=$(or $(TEMPLATE),base) cache=$(or $(CACHE),hot) n=$(or $(N),30)"
	go run ./cmd/pilldrops-bench -mode latency \
		-runtime "$(RUNTIME)" \
		-runner-class "$(RUNNER_CLASS)" \
		-template "$(or $(TEMPLATE),base)" \
		-cache "$(or $(CACHE),hot)" \
		-n "$(or $(N),30)" \
		$(if $(BENCH_HOST),-host "$(BENCH_HOST)",) \
		$(if $(NODE_ID),-node-id "$(NODE_ID)",) \
		$(if $(OUT),-out "$(OUT)",)

# The sandbox reachability/containment matrix, run rather than read. Needs the `probe` template built on the node
# (`sudo bash scripts/build-template.sh probe`) — probing from `base` cannot
# listen or send ICMP, so its results look like passes without being any.
#
#   RUNTIME  required — firecracker | docker | gvisor
#   STRICT=1 also fail when a probe could not run for any reason other than
#            "not applicable to this runtime". Use it in CI.
#
#   PILLDROPS_API_KEY=$$(sudo cat /var/lib/sandbox/state/bootstrap.key) \
#     make security-suite RUNTIME=firecracker STRICT=1
security-suite:
	@test -n "$(RUNTIME)" || (echo "RUNTIME= is required (firecracker|docker|gvisor)" && exit 2)
	@./scripts/security-suite.sh --runtime "$(RUNTIME)" \
		$(if $(BENCH_HOST_URL),--host "$(BENCH_HOST_URL)",) \
		$(if $(TEMPLATE),--template "$(TEMPLATE)",) \
		$(if $(STRICT),--strict,)

# The static half of the security gate: govulncheck, gosec, staticcheck and
# osv-scanner, gated on findings that are NOT in scripts/security-baseline.txt.
# Same script CI runs. Exit 2 means a scanner could not run — that is not a pass.
#
#   make security-scan                    # all four
#   make security-scan TOOLS="gosec"      # one
#   make security-scan-update             # rewrite the baseline (read the diff)
security-scan:
	@./scripts/security-scan.sh $(TOOLS)

security-scan-update:
	@./scripts/security-scan.sh --update

build-all:
	@echo "── Cross-compiling for macOS and Linux (compile guard)..."
	GOOS=darwin  GOARCH=arm64 go build ./...
	GOOS=darwin  GOARCH=amd64 go build ./...
	GOOS=linux   GOARCH=amd64 go build ./...
	@echo "── All platforms build"

lint:
	@echo "── Running linter..."
	@which golangci-lint > /dev/null 2>&1 || (echo "Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run ./...

# ─── Utilities ───────────────────────────────────────────────

.PHONY: clean logs status

clean:
	rm -rf bin/ screenshots/ .playwright-mcp/

logs:
ifndef HOST
	@journalctl -u sandboxd -f 2>/dev/null || echo "Run on the prod host or pass HOST=x.x.x.x"
else
	ssh root@$(HOST) "journalctl -u sandboxd -f"
endif

status:
ifndef HOST
	@systemctl status sandboxd 2>/dev/null || echo "Run on the prod host or pass HOST=x.x.x.x"
else
	ssh root@$(HOST) "systemctl status sandboxd"
endif

# ─── Help ────────────────────────────────────────────────────

.PHONY: help

help:
	@echo ""
	@echo "  PillDrops — Make Targets"
	@echo "  ════════════════════════"
	@echo ""
	@echo "  Development (any OS):"
	@echo "    make dev              Real sandbox server on :8080"
	@echo "    make serve            Rebuild the web UI, then run"
	@echo "    make sandboxd         Build the node agent for this machine"
	@echo "    make orchestrator     Build the control plane"
	@echo ""
	@echo "  Before you open a PR:"
	@echo "    make check            Everything CI runs. CHECK_HOST=user@box"
	@echo "                          runs the Linux half on a real KVM box;"
	@echo "                          STRICT=1 turns a skip into a failure."
	@echo "    make test             Go test suite"
	@echo "    make test-cover       ...with a coverage report"
	@echo "    make lint             golangci-lint"
	@echo "    make docs-check       Verify docs/agent/ matches the code"
	@echo ""
	@echo "  Database:"
	@echo "    make db-up            Start PostgreSQL (Docker)"
	@echo "    make db-down          Stop PostgreSQL"
	@echo "    make db-migrate       (no-op: the server self-migrates at startup)"
	@echo "    make db-reset         Wipe DB and re-migrate"
	@echo "    make db-shell         Open psql shell"
	@echo ""
	@echo "  Production build:"
	@echo "    make build            Node agent + envd + agentd (linux/amd64)"
	@echo "    make envd             envd only"
	@echo "    make proto            Regenerate protobuf stubs"
	@echo "    make template         Bake a template rootfs"
	@echo ""
	@echo "  Production run (local Linux):"
	@echo "    make prod-setup       Run host setup + build template"
	@echo "    make prod-run         Start the node with a DB"
	@echo ""
	@echo "  Production deploy (remote):"
	@echo "    make prod-deploy HOST=x.x.x.x          Deploy + restart"
	@echo "    make logs HOST=x.x.x.x                 Tail remote logs"
	@echo "    make status HOST=x.x.x.x               Check service status"
	@echo ""
	@echo "  Other:"
	@echo "    make clean            Remove build artifacts"
	@echo "    make help             Show this help"
	@echo ""

.DEFAULT_GOAL := help

# ─── Web UI (dashboard SPA) ──────────────────────────────────
.PHONY: web
# Local dashboard build: Clerk DISABLED.
#
# One build. There used to be a second target, web-local, that existed only
# because the production build baked in a live Clerk key which cannot
# authenticate against localhost — the app mounted its sign-in gate and span
# forever with no error. There is no identity provider now, so there is nothing
# to disable and nothing to get wrong.
web:
	@echo "── Building dashboard SPA…"
	@cd web && pnpm install --silent && pnpm build
	@rm -rf internal/webui/dist && cp -r web/dist internal/webui/dist
	@echo "── SPA built and synced into internal/webui/dist (rebuild sandboxd to embed)"
