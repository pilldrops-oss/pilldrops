#!/bin/sh
# Docker Engine inside the sandbox. init backgrounds this as a child of PID 1,
# so dockerd survives envd's process-group kills on exec'd commands.
set -e
mkdir -p /var/log

# /var/lib/docker CANNOT live on the guest's normal filesystem. The guest root
# is itself an overlayfs (internal/templatebuild/assets/overlay-init: lowerdir=/rom over the ext4's
# /overlay/upper), and overlayfs does not stack: dockerd starts, pulls, and then
# every container fails to mount with a bare EINVAL that reads like a kernel
# bug. overlay-init binds the raw ext4 at /mnt/disk for exactly this.
if [ -d /mnt/disk/upper ]; then
    DATA_ROOT=/mnt/disk/docker
else
    # Never fall back silently: an older rootfs whose overlay-init predates the
    # bind would otherwise hand the user a daemon that cannot run anything.
    echo "WARNING: /mnt/disk is not the raw ext4 — overlay-init is too old. Falling back to /var/lib/docker on overlayfs, where overlay2 cannot mount." >&2
    DATA_ROOT=/var/lib/docker
fi
mkdir -p "$DATA_ROOT"

# --allow-direct-routing: since v28 dockerd installs a DROP rule in iptables'
# `raw` table, and this kernel is built without CONFIG_IP_NF_RAW, so every
# container start fails on "can't initialize iptables table `raw'". The flag
# skips that rule. It costs nothing here: the only host that can reach the
# bridge is this guest, which already has root.
# ponytail: dockerd logs to one file with no rotation. logrotate only if a
# 20 GiB overlay actually fills; the images will get there first.
dockerd --data-root "$DATA_ROOT" --iptables=true --allow-direct-routing \
    >/var/log/dockerd.log 2>&1 &

# Hold start.sh open until the socket answers, so anything waiting on this
# script waits for a usable daemon rather than a spawned process.
i=0
while [ $i -lt 60 ]; do
    if docker version >/dev/null 2>&1; then
        echo "dockerd ready after $((i / 2))s, data root $DATA_ROOT"
        exit 0
    fi
    i=$((i + 1))
    sleep 0.5
done
echo "dockerd did not answer within 30s; see /var/log/dockerd.log" >&2
exit 1
