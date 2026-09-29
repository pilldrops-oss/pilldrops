#!/bin/sh
# Recording browser-lab: the preview port serves /root/recordings (videos,
# traces, HTML report) with a directory UI. pwtest / pwrecord write here.
PORT="${PORT:-8000}"
mkdir -p /root/recordings/videos
exec serve -l "tcp://0.0.0.0:${PORT}" /root/recordings
