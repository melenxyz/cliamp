package soundcloudx

import "strings"

// titleSeparators are what uploaders put between an artist and a title.
var titleSeparators = []string{" - ", " – ", " — ", ": ", " | "}

// cleanTitle drops a leading copy of artist from title, so a track that
// Forss uploaded as "Forss - Flickermood" reads "Flickermood". A title that
// would be empty without the prefix is kept as is.
func cleanTitle(title, artist string) string {
	title = strings.TrimSpace(title)
	artist = strings.TrimSpace(artist)
	if artist == "" {
		return title
	}
	lower := strings.ToLower(title)
	prefix := strings.ToLower(artist)
	for _, sep := range titleSeparators {
		if rest, ok := strings.CutPrefix(lower, prefix+sep); ok && strings.TrimSpace(rest) != "" {
			return strings.TrimSpace(title[len(title)-len(rest):])
		}
	}
	return title
}

// displayName is the row name of a track: "Artist – Title". The en dash
// keeps the separator apart from the hyphens many titles contain.
func displayName(artist, title string) string {
	title = cleanTitle(title, artist)
	if artist == "" {
		return title
	}
	return artist + " – " + title
}
