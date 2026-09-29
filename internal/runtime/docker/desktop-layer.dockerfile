RUN apt-get update && apt-get install -y --no-install-recommends \
      xvfb fluxbox x11vnc websockify xterm xdotool scrot autocutsel xclip x11-utils procps \
 && apt-get download novnc && dpkg-deb -x novnc_*.deb / && rm -f novnc_*.deb \
 && rm -rf /var/lib/apt/lists/*
RUN printf '%s' '<!doctype html><meta charset=utf-8><title>Desktop</title><script>var d=location.pathname.replace(/[^/]*$/,"");location.replace("vnc.html?autoconnect=true&resize=remote&reconnect=true&path="+encodeURIComponent((d+"websockify").replace(/^[/]/,"")));</script><noscript>Open <a href=vnc.html?autoconnect=true>vnc.html</a>.</noscript>' > /usr/share/novnc/index.html
