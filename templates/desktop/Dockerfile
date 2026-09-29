FROM debian:bookworm-slim

# The screen itself — Xvfb, fluxbox, x11vnc, noVNC — is the shared desktop
# layer scripts/build-template.sh adds to every template image, and envd starts
# it on PD_DESKTOP=1. What is left here is the browser and the autostart that
# fills the display, which is what makes this a *desktop* template rather than a
# machine that could have one.
RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium dbus-x11 \
    fonts-dejavu-core curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Baked, not asked for at create: this template IS the desktop. envd reads it
# from /etc/sandbox-env at boot (Firecracker) or from the container env (Docker).
ENV PD_DESKTOP=1
ENV DISPLAY=:0

# start.sh (auto-started by init) waits for envd's display and launches
# Chromium onto it; the viewer is envd's noVNC on the desktop port.

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh
