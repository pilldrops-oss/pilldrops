//go:build !linux

package main

// seedEntropy is a no-op off Linux.
func seedEntropy() {}
