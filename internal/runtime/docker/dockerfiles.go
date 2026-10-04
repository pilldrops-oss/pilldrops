package docker

import (
	_ "embed"
	"strings"
)

// desktopLayerFile is the ONE copy of the screen stack. Embedded for the Docker
// runtime, read by scripts/build-template.sh for the Firecracker rootfs, and
// the package list scripts/verify-templates.sh probes is derived from it, so
// the three cannot drift.
//
//go:embed desktop-layer.dockerfile
var desktopLayerFile string

// DesktopLayer is the on-demand screen stack, appended to every template image
// the platform builds so PD_DESKTOP=1 works in any sandbox rather than only in
// the templates that bake a desktop of their own.
//
// Measured on a node, not estimated: 486 MB of image layer, 473 MB on disk
// (node:20-slim, 2026-09-20). It costs disk, and nothing at runtime until envd
// is asked for a display. The proposal's "~60 MB" was out by a factor of eight
// and the difference is not trimmable with Debian packages: xvfb Depends on
// libgl1 for GLX, which pulls libgl1-mesa-dri and libllvm15 (159 MB of
// software GL), and fluxbox pulls libimlib2 with its Ghostscript, poppler and
// x265 loaders (76 MB). tigervnc-standalone-server, the resize fallback task
// 3.3 holds in reserve, Depends on libgl1 too and is 68 MB *larger*, so
// switching to it is a resize decision taken knowing it costs more — never a
// size regression anyone should be surprised by later.
//
// xclip and x11-utils are 0 MB and 1 MB on top of the rest, and this change's
// own verification needs both (xclip to drive a clipboard round trip,
// xdpyinfo to read the display size). A capability nothing in the image can
// exercise is a claim rather than a feature.
//
// ponytail: this layer in every template image, because an image is the only
// place either runtime can find these binaries. Accepted deliberately.
//
// 486 MB is the UNCOMPRESSED container layer. What a Firecracker node
// actually fetches is the zstd squashfs, and that was measured A/B with
// PD_NO_DESKTOP=1 as the control on the same script (2026-09-20):
//
//	base      67 MiB -> 219 MiB   delta 152 MiB  (+227%)
//	desktop  314 MiB -> 381 MiB   delta  68 MiB  (+21.6%)
//
// So the real per-template cost is 68-152 MiB, roughly a third of the
// container figure, and it shrinks the more X the template already had —
// `desktop` shares most of the dependency closure already. 31 templates at
// the worst case is ~4.7 GB, well under 1% of a node's disk.
//
// The cost to optimise is still FETCH rather than disk: Docker stores
// identical content once per node however many images append it, while every
// *cold* Firecracker template fetch grows by up to 152 MiB, which is
// first-boot latency and artifact-store traffic. The upgrade path, if that
// ever bites, is one shared read-only layer mounted on demand (a second
// squashfs under overlay-init on Firecracker, a shared volume on Docker),
// fetched once per node instead of once per template — a change to how
// sandboxes are composed, not to this line.
//
// Deliberately not tolerant of failure: an image where this cannot install is
// an image whose has_desktop claim would be a lie, and a loud build failure
// falls back to the un-enriched base rather than shipping a silent one.
//
// noVNC is unpacked from its own .deb rather than installed, because the
// Debian `novnc` package Depends on nodejs, net-tools, python3-numpy and
// python3-novnc (which drags in python3-pil and the OpenStack oslo stack) to
// ship 248 KB of static HTML and JavaScript that websockify serves. Measured
// on the node: installing it costs 183 MB and a 583 MB layer; unpacking the
// same package from the same mirror at the same version costs 248 KB and a
// 486 MB layer. Those dependencies are for the `novnc` launcher script, which
// nothing here runs.
//
// The landing page in the layer exists because noVNC ships vnc.html but no
// index.html, so the desktop port's root would 404. Written by the image build
// rather than by envd: envd runs unprivileged and cannot write
// /usr/share/novnc. resize=remote is what makes the display follow the browser
// window; Xvfb's RANDR extension and x11vnc -xrandr are the other two thirds.
var DesktopLayer = "\n" + desktopLayerFile

// enrichedDockerfiles bakes each template's advertised tooling onto its base
// image, built once per node. Keyed by canonical template id.
var enrichedDockerfiles = map[string]string{
	"code-interpreter": `FROM python:3.11-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends curl git build-essential && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir numpy pandas matplotlib scipy scikit-learn requests
WORKDIR /workspace`,
	// The demo config and spec mirror what the firecracker rootfs bakes, so the
	// dashboard's demo launch works on both runtimes.
	"playwright": `FROM mcr.microsoft.com/playwright:v1.49.0-jammy
RUN npm install -g @playwright/test@1.49.0
WORKDIR /workspace
RUN printf '%s\n' \
  'module.exports = {' \
  '  testDir: "./tests",' \
  '  reporter: [["list"], ["html", { open: "never" }]],' \
  '  use: { headless: true, video: "on", trace: "on", screenshot: "on" },' \
  '};' \
  > playwright.config.js && mkdir -p tests && printf '%s\n' \
  'const { test, expect } = require("@playwright/test");' \
  'test("example.com renders", async ({ page }) => {' \
  '  await page.goto("https://example.com");' \
  '  await expect(page).toHaveTitle(/Example/);' \
  '});' \
  > tests/example.spec.js`,

	// Mirrors templates/node/Dockerfile.
	"node": `FROM node:20-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl git ca-certificates && rm -rf /var/lib/apt/lists/*
RUN npm install -g typescript tsx ts-node
WORKDIR /workspace`,

	// Desktop for computer-use agents. The screen itself is DesktopLayer,
	// which every template image now carries and envd starts on PD_DESKTOP=1;
	// what is left here is the browser and the autostart that puts it on the
	// display, which is what makes this template a *desktop* rather than a
	// machine that could have one.
	"desktop": `FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium dbus-x11 curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# Chromium onto the display envd brought up. Written here because an inline
# Dockerfile has no build context to COPY from; the Firecracker twin is
# templates/desktop/start.sh.
RUN printf '%s\n' \
  '#!/bin/sh' \
  '# PD_DESKTOP=1 is baked into this template, so envd owns Xvfb, fluxbox,' \
  '# x11vnc and websockify. This waits for that display and fills it.' \
  'export DISPLAY=:0 HOME=/root' \
  'URL="${DESKTOP_URL:-https://www.google.com}"' \
  'for i in $(seq 1 100); do [ -e /tmp/.X11-unix/X0 ] && break; sleep 0.3; done' \
  'while :; do rm -f /root/.chrome/Singleton* 2>/dev/null; chromium --no-sandbox --no-first-run --disable-infobars --disable-gpu --disable-session-crashed-bubble --start-maximized --user-data-dir=/root/.chrome "$URL" >/dev/null 2>&1; sleep 2; done' \
  > /usr/local/bin/start.sh && chmod +x /usr/local/bin/start.sh
# Baked, not asked for at create: this template IS the desktop, so envd starts
# the stack from the container env. The noVNC landing page is envd's too.
ENV PD_DESKTOP=1
ENV DISPLAY=:0
WORKDIR /workspace`,
}

// canonicalTemplate maps a template name or alias to the id keying enrichedDockerfiles.
func canonicalTemplate(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "code-interpreter", "python-3.11", "python-data-science", "code-interpreter-v1":
		return "code-interpreter"
	case "playwright", "playwright-test", "e2e":
		return "playwright"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}
