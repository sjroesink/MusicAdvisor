package musicbrainz

import "strings"

// CoverArtBase is the Cover Art Archive's public host. Split out so tests
// can point at a local mock.
const CoverArtBase = "https://coverartarchive.org"

// CoverArtURL returns a stable URL to the front cover art for a release-
// group. The URL 404s when no art exists; the frontend renders an <img>
// with an error-fallback, so we don't pre-HEAD the URL from the server.
//
// size can be 250, 500, 1200, or "" for the full-resolution default.
// Callers usually pass 500 for list thumbnails.
func CoverArtURL(releaseGroupMBID string, size int) string {
	if releaseGroupMBID == "" {
		return ""
	}
	path := "/release-group/" + releaseGroupMBID + "/front"
	switch size {
	case 250, 500, 1200:
		path += "-" + itoaSmall(size)
	}
	return CoverArtBase + path
}

// itoaSmall avoids the strconv import for this single hot call.
func itoaSmall(n int) string {
	switch n {
	case 250:
		return "250"
	case 500:
		return "500"
	case 1200:
		return "1200"
	}
	return ""
}

// IsCoverArtURL recognizes a URL we produced so the frontend can tell
// "backend-supplied cover art" from older "initials placeholder" strings.
func IsCoverArtURL(s string) bool {
	return strings.HasPrefix(s, CoverArtBase)
}
