package soundcloudx

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/ytdlcookies"
)

// Flat listings of artist pages and likes carry no durations. A full
// yt-dlp extraction has them but costs about a second per track, so the
// provider fills them in the background after a list opens, a few tracks
// per yt-dlp call, and keeps them in the index: each track is looked up once.
const (
	durationBatch   = 10  // track URLs per yt-dlp call
	durationWorkers = 3   // yt-dlp calls at a time
	durationCap     = 150 // tracks per list open; the rest fill on later opens
)

// durationFetcher looks up durations. Tests replace it.
type durationFetcher func(ctx context.Context, perms []string) ([]Meta, error)

// durationState tracks the lookups in flight, so a list that opens twice
// does not fetch twice.
type durationState struct {
	mu       sync.Mutex
	inflight map[string]bool
}

// fillDurations starts a background lookup of the tracks among keys that
// have no duration. When it finishes, the footer says so; Ctrl+R then shows
// the durations in the open list.
func (p *Provider) fillDurations(keys []string) {
	if p.fetchDurations == nil {
		return
	}
	var todo []Entry
	p.durations.mu.Lock()
	if p.durations.inflight == nil {
		p.durations.inflight = map[string]bool{}
	}
	for _, k := range keys {
		e, ok := p.store.Get(k)
		if !ok || e.Duration > 0 || e.Permalink == "" || p.durations.inflight[k] {
			continue
		}
		p.durations.inflight[k] = true
		todo = append(todo, e)
		if len(todo) == durationCap {
			break
		}
	}
	p.durations.mu.Unlock()
	if len(todo) == 0 {
		return
	}

	applog.Status("Fetching durations for %d tracks…", len(todo))
	go func() {
		batches := make(chan []Entry)
		var wg sync.WaitGroup
		var mu sync.Mutex
		found := 0
		for range durationWorkers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for batch := range batches {
					perms := make([]string, len(batch))
					for i, e := range batch {
						perms[i] = e.Permalink
					}
					metas, err := p.fetchDurations(p.life, perms)
					if err != nil && p.life.Err() == nil {
						applog.Warn("soundcloudx: durations: %v", err)
					}
					if len(metas) > 0 {
						_ = p.store.Update(metas...)
						mu.Lock()
						found += len(metas)
						mu.Unlock()
					}
				}
			}()
		}
	send:
		for i := 0; i < len(todo); i += durationBatch {
			select {
			case batches <- todo[i:min(i+durationBatch, len(todo))]:
			case <-p.life.Done():
				break send
			}
		}
		close(batches)
		wg.Wait()

		p.durations.mu.Lock()
		for _, e := range todo {
			delete(p.durations.inflight, e.Key)
		}
		p.durations.mu.Unlock()
		if found > 0 && p.life.Err() == nil {
			applog.Status("Durations ready for %d tracks · Ctrl+R to show them", found)
		}
	}()
}

// ytdlpDurations runs one full yt-dlp extraction (no download) over a few
// track pages and returns their durations and uploader names.
func ytdlpDurations(ctx context.Context, perms []string) ([]Meta, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(perms))*15*time.Second)
	defer cancel()
	args := []string{
		"--skip-download", "--ignore-errors", "--no-warnings", "--no-playlist",
		"--socket-timeout", "15",
		"--print", "%(id)s\t%(webpage_url)s\t%(duration)s\t%(uploader)s",
	}
	if b := ytdlcookies.ForURL(pageBase); b != "" {
		args = append(args, "--cookies-from-browser", b)
	}
	args = append(args, "--")
	for _, perm := range perms {
		args = append(args, pageURL(perm))
	}
	cmd := exec.CommandContext(ctx, ytdlpBin, args...)
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.Output()
	metas := parseDurations(string(out))
	if err != nil && len(metas) == 0 {
		return nil, fmt.Errorf("yt-dlp: %w", err)
	}
	return metas, nil
}

// parseDurations reads the tab-separated lines that ytdlpDurations prints.
func parseDurations(out string) []Meta {
	var metas []Meta
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), "\t", 4)
		if len(f) != 4 {
			continue
		}
		perm := permalinkPath(f[1])
		secs, err := strconv.ParseFloat(f[2], 64)
		if perm == "" || err != nil || secs <= 0 {
			continue
		}
		uploader := f[3]
		if uploader == "NA" {
			uploader = ""
		}
		metas = append(metas, Meta{Key: trackKey(f[0], perm), Permalink: perm, Artist: uploader, Duration: int(secs)})
	}
	return metas
}
