package soundcloudx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/bjarneo/cliamp/internal/ytdlcookies"
)

// listTimeout bounds one listing. Likes of a big account take a while.
const listTimeout = 2 * time.Minute

// flatEntry holds the fields of a yt-dlp --flat-playlist -j line.
type flatEntry struct {
	ID         string  `json:"id"`
	URL        string  `json:"url"`
	WebpageURL string  `json:"webpage_url"`
	Title      string  `json:"title"`
	Uploader   string  `json:"uploader"`
	Duration   float64 `json:"duration"`
}

// lister lists a SoundCloud page or search. Tests replace it.
type lister func(ctx context.Context, target string) ([]Meta, error)

// ytdlpList lists target with yt-dlp --flat-playlist. target is a page URL
// or a scsearchN: query.
func ytdlpList(ctx context.Context, target string) ([]Meta, error) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return nil, fmt.Errorf("yt-dlp not found in PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	args := []string{"--flat-playlist", "-j", "--socket-timeout", "15"}
	if b := ytdlcookies.ForURL(pageBase); b != "" {
		args = append(args, "--cookies-from-browser", b)
	}
	args = append(args, "--", target)
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	cmd.WaitDelay = 3 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("yt-dlp: list %s: %w", target, ctx.Err())
		}
		if msg := lastLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("yt-dlp: %s", msg)
		}
		return nil, fmt.Errorf("yt-dlp: %w", err)
	}
	return parseFlat(bytes.NewReader(out))
}

// parseFlat reads yt-dlp --flat-playlist -j output. Entries that are not
// SoundCloud tracks (sets inside a likes list, for example) are skipped.
func parseFlat(r io.Reader) ([]Meta, error) {
	var out []Meta
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e flatEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		page := e.WebpageURL
		if page == "" {
			page = e.URL
		}
		perm := permalinkPath(page)
		if perm == "" || strings.Contains(perm, "/sets/") {
			continue
		}
		title := e.Title
		if title == "" {
			_, slug, _ := strings.Cut(perm, "/")
			title = strings.ReplaceAll(slug, "-", " ")
		}
		artist := e.Uploader
		if artist == "" {
			artist = artistFromPermalink(perm)
		}
		out = append(out, Meta{
			Key:       trackKey(e.ID, perm),
			Permalink: perm,
			Title:     title,
			Artist:    artist,
			Duration:  int(e.Duration),
		})
	}
	return out, sc.Err()
}

// lastLine returns the last non-empty line of s, which is where yt-dlp puts
// its ERROR message.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
