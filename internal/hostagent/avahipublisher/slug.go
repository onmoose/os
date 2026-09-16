// Package avahipublisher — slug validation shared across all platforms.
package avahipublisher

import (
	"regexp"
	"strings"
)

// slugRE is the valid slug pattern — same character class the catalog/manifest
// layer enforces. Defensive: rejects injection attempts before we ever touch
// DBus or the filesystem.
var slugRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// sanitizeBoxLabel reduces a host's name to a single DNS label for use in the
// collision-fallback name ("<slug>-<box>.local"): it takes the first
// dot-separated component, lowercases it, and drops any character outside
// [a-z0-9-]. Returns "moose" if the result is empty (e.g. an empty or
// all-punctuation hostname).
func sanitizeBoxLabel(h string) string {
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	h = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, h)
	if h == "" {
		return "moose"
	}
	return h
}
