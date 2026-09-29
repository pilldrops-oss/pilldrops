//go:build !linux

package main

// applyLimits is a no-op off Linux; the stub keeps the package testable on dev machines.
func applyLimits(int) {}
