# Headed Playwright: same toolchain as the playwright image plus a virtual
# display served over noVNC — watch tests run live AND get recordings.
# NOTE: build the `playwright` template first on the same host; this layers
# on its Docker image (build-template.sh tags it sandbox-playwright).
FROM sandbox-playwright

# The screen — Xvfb, fluxbox, x11vnc, noVNC, websockify — is the shared desktop
# layer, and envd starts it on PD_DESKTOP=1, on the platform desktop port. This
# image only adds what makes the display worth watching: headed Playwright and
# the recordings served beside the viewer.
ENV PD_DESKTOP=1
ENV DISPLAY=:0

# PW_HEADED flips the baked playwright config + pwrecord to headed mode; the
# browser windows appear on the display noVNC streams.
ENV PW_HEADED=1

# Videos, report and traces are reachable from the viewer's own server at
# /recordings/. The landing page is the desktop layer's — a copy here would be
# overwritten by it, since the layer is applied after this build.
RUN ln -s /root/recordings /usr/share/novnc/recordings

# The envd binary and overlay-init are injected by build-template.sh
