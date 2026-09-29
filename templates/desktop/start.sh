#!/bin/sh
# Browser sandbox: Chromium on the display envd brought up, watched through
# noVNC on the desktop port. envd owns Xvfb, fluxbox, x11vnc and websockify
# (PD_DESKTOP=1 is baked into this image); this only fills the screen.
export DISPLAY=:0
export HOME=/root
URL="${DESKTOP_URL:-https://www.google.com}"

# envd starts the display in parallel with init running this, so wait for it
# rather than racing it. 30s: a cold boot has an apt-free but still busy guest.
for i in $(seq 1 100); do [ -e /tmp/.X11-unix/X0 ] && break; sleep 0.3; done

# Relaunched on exit so closing Chromium from the window manager leaves a
# usable desktop; the rm clears a stale profile lock that blocks relaunch.
# --start-maximized, not a fixed --window-size: the display follows the
# viewer's window (resize=remote), so a baked size would be wrong after the
# first resize.
while :; do
    rm -f /root/.chrome/Singleton* 2>/dev/null
    chromium --no-sandbox --no-first-run --disable-infobars \
        --disable-session-crashed-bubble --disable-gpu \
        --start-maximized --user-data-dir=/root/.chrome "$URL" >/dev/null 2>&1
    sleep 2
done
