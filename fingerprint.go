package main

import "strings"

// fingerprint identifies a saved song, as Rails' SavedSong.build_fingerprint:
// the external ID when there is one, else the normalized title, artist, and version.
func fingerprint(title, artist, version, externalID string) string {
	if id := strings.TrimSpace(externalID); id != "" {
		return "ext:" + id
	}
	return "norm:" + normalizeToken(title) + "|" + normalizeToken(artist) + "|" + normalizeToken(version)
}

// normalizeToken lowercases, trims, and collapses whitespace. Punctuation is
// kept, so "Don't" and "Dont" stay different songs.
func normalizeToken(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// songKey matches a song regardless of version, for showing a saved heart.
func songKey(title, artist string) string {
	return normalizeToken(title) + "|" + normalizeToken(artist)
}
