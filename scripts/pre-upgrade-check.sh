#!/usr/bin/env bash
# pre-upgrade-check.sh: the machine-checkable half of the pre-upgrade
# checklist. Items that cannot be answered from inside a node are printed as
# manual reminders at the end of the run.
#
# Exit codes:
#   0  every automated check passed
#   1  a check failed
#   2  a check could not be run at all; not a pass
#
# Usage:  sudo scripts/pre-upgrade-check.sh
set -uo pipefail

PASS=0
FAIL=0
SKIP=0

ok()   { echo "PASS  $*"; PASS=$((PASS+1)); }
bad()  { echo "FAIL  $*"; FAIL=$((FAIL+1)); }
skip() { echo "SKIP  $* (this is not a pass)"; SKIP=$((SKIP+1)); }

echo "== pre-upgrade check =="
echo

# `cosign version` catches an install that is on PATH but broken.
if command -v cosign >/dev/null 2>&1 && cosign version >/dev/null 2>&1; then
  ok "cosign present ($(cosign version 2>/dev/null | awk '/GitVersion/{print $2}' | head -1))"
else
  bad "cosign is missing or not runnable — sandboxd-update will refuse to install anything"
fi

if [ -f /etc/sandboxd-update.env ]; then
  mode=$(stat -c '%a' /etc/sandboxd-update.env 2>/dev/null || stat -f '%OLp' /etc/sandboxd-update.env)
  owner=$(stat -c '%U' /etc/sandboxd-update.env 2>/dev/null || stat -f '%Su' /etc/sandboxd-update.env)
  if [ "$mode" = "600" ] && [ "$owner" = "root" ]; then
    ok "/etc/sandboxd-update.env is 600 root"
  else
    # Whoever can write this file chooses who may sign the next root-installed binary.
    bad "/etc/sandboxd-update.env is $mode $owner — must be 600 root (it pins the signer identity)"
  fi
else
  bad "/etc/sandboxd-update.env missing — no pinned signer identity for sandboxd-update"
fi

# NODE_KEY_SECRET must never leave the control plane, so absence on a node is correct.
if [ -n "${ORCHESTRATOR_URL:-}" ] && [ -z "${NODE_KEY_SECRET:-}" ]; then
  ok "NODE_KEY_SECRET is not present on this node (correct — it belongs to the control plane only)"
elif [ -n "${NODE_KEY_SECRET:-}" ]; then
  if [ "${NODE_KEY_SECRET}" = "${ORCHESTRATOR_TOKEN:-}" ]; then
    bad "NODE_KEY_SECRET equals ORCHESTRATOR_TOKEN — every node holds that, so it derives every other node's key"
  else
    ok "NODE_KEY_SECRET is set and differs from ORCHESTRATOR_TOKEN"
  fi
else
  skip "NODE_KEY_SECRET — not a control plane and ORCHESTRATOR_URL unset; check it where the orchestrator runs"
fi

if command -v systemctl >/dev/null 2>&1 && systemctl cat sandboxd >/dev/null 2>&1; then
  # Check effective values so a drop-in override fails too. systemd reports an
  # unset TasksMax as DefaultTasksMax, a number, not "infinity".
  DEFAULT_TASKS=$(systemctl show -p DefaultTasksMax --value 2>/dev/null)

  val=$(systemctl show sandboxd -p MemoryMax --value 2>/dev/null)
  case "$val" in
    infinity|"") bad "sandboxd MemoryMax=$val — the unit was not reloaded, or a drop-in overrides it" ;;
    *)           ok "sandboxd MemoryMax=$val" ;;
  esac

  val=$(systemctl show sandboxd -p TasksMax --value 2>/dev/null)
  if [ -z "$val" ] || [ "$val" = "infinity" ]; then
    bad "sandboxd TasksMax=$val"
  elif [ -n "$DEFAULT_TASKS" ] && [ "$val" = "$DEFAULT_TASKS" ]; then
    bad "sandboxd TasksMax=$val is the host default (DefaultTasksMax) — the unit sets no TasksMax of its own"
  else
    ok "sandboxd TasksMax=$val (host default is ${DEFAULT_TASKS:-unknown})"
  fi

  val=$(systemctl show sandboxd -p NoNewPrivileges --value 2>/dev/null)
  case "$val" in
    yes) ok "sandboxd NoNewPrivileges=yes" ;;
    *)   bad "sandboxd NoNewPrivileges=$val" ;;
  esac
else
  skip "systemd unit properties — no systemctl, or no sandboxd unit installed"
fi

# The daemon's MemoryMax does not cover the guests. Zero cgroups is a skip, not a pass.
FCG=/sys/fs/cgroup/firecracker
if [ -d "$FCG" ]; then
  leaves=0
  unlimited=0
  for d in "$FCG"/*/; do
    [ -d "$d" ] || continue
    leaves=$((leaves+1))
    m=$(cat "$d/memory.max" 2>/dev/null || echo missing)
    p=$(cat "$d/pids.max" 2>/dev/null || echo missing)
    if [ "$m" = "max" ] || [ "$m" = "missing" ] || [ "$p" = "max" ] || [ "$p" = "missing" ]; then
      bad "VMM cgroup $(basename "$d"): memory.max=$m pids.max=$p — the VM is running uncapped"
      unlimited=$((unlimited+1))
    fi
  done
  if [ "$leaves" -eq 0 ]; then
    skip "per-VMM cgroup limits — no VM is running, so there is nothing to read"
  elif [ "$unlimited" -eq 0 ]; then
    ok "$leaves VMM cgroup(s), all with memory.max and pids.max set"
  fi
else
  skip "per-VMM cgroup limits — $FCG does not exist (no VM has run since boot)"
fi

# Not checkable without creating a sandbox on a live node.
echo "INFO  tenant code now runs as uid 1001: 'apt-get install' inside a plain sandbox fails."
echo "      Verify with a live sandbox:  sandbox exec <id> -- id -u   (expect 1001)"

echo
echo "MANUAL, not checked here:"
echo "  1. Re-run scripts/postgres-node-role.sql against the control-plane database."
echo "     An earlier version aborted after its blanket GRANT and before any revoke,"
echo "     leaving the node role write access to every table including vault_secrets."
echo "  5. IMDSv2 with --http-put-response-hop-limit 1 on cloud nodes."

echo
echo "== $PASS passed, $FAIL failed, $SKIP could not run =="
[ "$FAIL" -gt 0 ] && exit 1
[ "$SKIP" -gt 0 ] && exit 2
exit 0
