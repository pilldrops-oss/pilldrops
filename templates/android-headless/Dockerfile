# Headless Android 14 emulator for CI/E2E — same prebuilt image as the
# `android` template (see builtinServices in internal/runtime/docker), but the
# VNC/noVNC streaming stack never starts and the primary published port is
# Appium (4723): the sandbox's preview URL is a remote WebDriver endpoint.
# Requires a KVM-capable node; shares the pilldrops/android-warm bake.
FROM budtmo/docker-android:emulator_14.0
