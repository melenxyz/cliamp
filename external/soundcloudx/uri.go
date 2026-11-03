package soundcloudx

import (
	"net/url"
	"regexp"
	"strings"
)

// URIScheme prefixes the path of every track this provider lists. The rest
// is "<key>/<permalink path>", e.g. "soundcloudx:49437906/forss/veniam", so
// the provider can find the index entry and rebuild the page URL without I/O.
const URIScheme = "soundcloudx:"

// MetaID is the ProviderMeta key that holds the track key.
const MetaID = "soundcloudx.id"

const pageBase = "https://soundcloud.com/"

var unsafeKeyChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// trackKey returns the key a track is stored under: the SoundCloud ID when
// yt-dlp reports one, otherwise a key derived from the permalink path.
func trackKey(id, permalink string) string {
	if id = strings.TrimSpace(id); id != "" && unsafeKeyChars.FindStringIndex(id) == nil {
		return id
	}
	return "p-" + strings.Trim(unsafeKeyChars.ReplaceAllString(permalink, "-"), "-")
}

// permalinkPath returns the path of a soundcloud.com page URL without the
// leading slash, query and fragment, or "" when pageURL is not on SoundCloud.
func permalinkPath(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	host = strings.TrimPrefix(host, "m.")
	if host != "soundcloud.com" {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

// buildURI returns the track path for key and permalink.
func buildURI(key, permalink string) string {
	return URIScheme + key + "/" + permalink
}

// parseURI splits a track path into its key and permalink path.
func parseURI(uri string) (key, permalink string, ok bool) {
	rest, ok := strings.CutPrefix(uri, URIScheme)
	if !ok {
		return "", "", false
	}
	key, permalink, _ = strings.Cut(rest, "/")
	if key == "" {
		return "", "", false
	}
	return key, permalink, true
}

// pageURL returns the soundcloud.com URL for a permalink path.
func pageURL(permalink string) string { return pageBase + permalink }

// artistFromPermalink returns the user segment of a permalink path, which
// stands in for the artist until a download reports the uploader's name.
func artistFromPermalink(permalink string) string {
	user, _, _ := strings.Cut(permalink, "/")
	return user
}
