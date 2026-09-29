//go:build core

package httpapi

// functionFeature carries nothing in the core build; Server embeds it in both builds.
type functionFeature struct{}
