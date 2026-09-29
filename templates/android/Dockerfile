# Android 14 emulator sandbox — mobile E2E testing & app agents.
#
# The docker runtime boots this prebuilt image directly (see builtinServices in
# internal/runtime/docker/docker.go): KVM-accelerated Android 14 emulator with
# a browser-viewable screen (noVNC on 6080), Appium (4723), and adb inside.
# Requires a KVM-capable node (--device /dev/kvm).
#
# This Dockerfile records the image's provenance and keeps the catalog
# invariant that every built-in template has one.
FROM budtmo/docker-android:emulator_14.0
