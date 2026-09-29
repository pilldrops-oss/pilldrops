#!/bin/sh
# Headed Playwright lab. envd owns Xvfb, fluxbox, x11vnc and websockify
# (PD_DESKTOP=1 is baked into this image); this only makes the artifact
# directory the viewer serves at /recordings/, before any test writes to it.
mkdir -p /root/recordings/videos
