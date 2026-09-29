#!/bin/sh
# Browser terminal on the preview port. ttyd is spawned by init with a bare
# environment, so source the image's baked PATH/env and hand ttyd an absolute
# interpreter — otherwise its execvp("bash") fails (pty_spawn: ENOENT) and the
# terminal shows "Press Enter to Reconnect" forever.
[ -f /etc/sandbox-env ] && . /etc/sandbox-env
export PATH="${PATH:-/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}"
PORT="${PORT:-8000}"
cd /root
# -W makes the terminal writable (ttyd defaults to read-only). Each connection
# gets a fresh login shell; run `claude` to start Claude Code.
exec ttyd -p "$PORT" -W /usr/bin/bash -l
