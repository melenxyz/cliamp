// Package soundcloudx is a SoundCloud provider that keeps what it plays.
//
// It browses like external/soundcloud, but its tracks carry soundcloudx: URIs
// that the provider decodes itself (provider.CustomStreamer). The first play
// of a track streams from the file yt-dlp is writing, so the track is on disk
// once it has played. Every track is in one of three states:
//
//   - remote: never played or downloaded
//   - cached: played once; evicted least recently played first when the
//     cache is over cache_max
//   - pinned: held by at least one pin source (a sync rule or a manual pin);
//     never evicted and not counted toward cache_max
//
// SPEC.md at the repo root is the design document.
package soundcloudx

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultCacheMax is the cache limit when cache_max is not set.
const DefaultCacheMax int64 = 10 << 30 // 10 GiB

// DefaultSyncInterval is how often the sync rules run when sync_interval is
// not set.
const DefaultSyncInterval = 6 * time.Hour

// Config holds the settings of the [soundcloudx] section.
type Config struct {
	Enabled      bool
	User         string        // SoundCloud username for Likes, Tracks and Reposts. Optional.
	CookiesFrom  string        // browser name for yt-dlp --cookies-from-browser. Optional.
	Dir          string        // storage root; default ~/Music/soundcloud
	CacheMax     int64         // bytes; 0 means DefaultCacheMax
	Artists      []string      // artist usernames listed in the Artists section
	Sync         []string      // pin rules: "likes", "artist:<username>"
	SyncInterval time.Duration // 0 means DefaultSyncInterval; negative turns periodic sync off
}

// IsSet reports whether the provider should register.
func (c Config) IsSet() bool { return c.Enabled }

// root returns the storage root, expanding a leading ~.
func (c Config) root() (string, error) {
	dir := strings.TrimSpace(c.Dir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Music", "soundcloud"), nil
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("soundcloudx: dir must be an absolute path, got %q", c.Dir)
	}
	return filepath.Clean(dir), nil
}

// ParseSize parses a size such as "20GB", "512 MB", "1.5G" or a plain byte
// count. Units are binary (1 GB = 1024³ bytes), as disk tools report them.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') && s[i-1] != '.' {
		i--
	}
	num, unit := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i:])
	unit = strings.TrimSuffix(strings.TrimSuffix(unit, "B"), "I")
	var mult float64
	switch unit {
	case "":
		mult = 1
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * mult), nil
}

// FormatSize renders n bytes for display, e.g. "1.2 GB".
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
