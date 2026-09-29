# Mobile Test Session — zero-setup mobile E2E with automatic recording + report.
#
# The docker runtime boots this prebuilt image directly (see builtinServices in
# internal/runtime/docker/docker.go) and injects the session harness at boot
# (internal/runtime/docker/session_harness): a session server on 8765 (the
# preview URL — live progress while the test runs, full report with video and
# artifacts after) plus a watcher that auto-runs /test/run.sh screen-recorded.
# Headless (no VNC stack); Appium (4723) and adb run inside for test scripts.
# Requires a KVM-capable node (--device /dev/kvm).
#
# This Dockerfile records the image's provenance and keeps the catalog
# invariant that every built-in template has one.
FROM budtmo/docker-android:emulator_14.0
