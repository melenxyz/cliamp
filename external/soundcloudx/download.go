package soundcloudx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/ytdlcookies"
)

// audioFormat picks SoundCloud's HLS AAC streams, best first. SoundCloud no
// longer serves progressive MP3, and the original upload format needs a
// signed-in account, so bestaudio is the fallback.
const audioFormat = "hls_aac_160k/hls_aac_96k/bestaudio"

// ytdlpBin is the yt-dlp executable. Tests point it at a fake.
var ytdlpBin = "yt-dlp"

// downloadInfo is the JSON that the download prints before it starts and
// after the file is in place.
type downloadInfo struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Uploader string  `json:"uploader"`
	Duration float64 `json:"duration"`
	Ext      string  `json:"ext"`
	Filepath string  `json:"filepath"`
}

// job is one running download. A track has at most one job at a time;
// playback and sync share it.
type job struct {
	key  string
	meta Meta

	cancel    context.CancelFunc
	infoReady chan struct{} // closed once info and partPath are set, or at done
	done      chan struct{} // closed when the job ended; err and file are final

	info     downloadInfo
	partPath string // the .part file yt-dlp writes
	err      error
	file     string // the committed file on success

	nameMu        sync.Mutex
	artist, title string // as the download reported them

	// Guarded by downloader.mu.
	players int  // streamers reading this download
	keep    bool // a pin wants the file: finish even when no one listens
}

// Info returns the metadata the download reported, once infoReady is closed.
func (j *job) Info() downloadInfo { return j.info }

// downloader runs yt-dlp downloads into the partial directory and commits
// finished files to the store.
type downloader struct {
	store *Store

	mu   sync.Mutex
	jobs map[string]*job
	wg   sync.WaitGroup
}

func newDownloader(store *Store) *downloader {
	return &downloader{store: store, jobs: map[string]*job{}}
}

// play starts or joins m's download for a streamer. The streamer calls
// release when it closes; a download that only streamers want stops then,
// and its partial file is kept for a later resume.
func (d *downloader) play(m Meta) *job {
	return d.start(m, true)
}

// pin starts or joins m's download so it completes whether or not anyone
// listens.
func (d *downloader) pin(m Meta) *job {
	return d.start(m, false)
}

func (d *downloader) start(m Meta, player bool) *job {
	d.mu.Lock()
	defer d.mu.Unlock()
	if j, ok := d.jobs[m.Key]; ok {
		if player {
			j.players++
		} else if !j.keep {
			j.keep = true
			applog.Status("↓ Pinning %s…", j.name())
		}
		return j
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{
		key:       m.Key,
		meta:      m,
		cancel:    cancel,
		infoReady: make(chan struct{}),
		done:      make(chan struct{}),
		keep:      !player,
	}
	if player {
		j.players = 1
	}
	d.jobs[m.Key] = j
	d.store.Hold(m.Key)
	d.wg.Add(1)
	go d.run(ctx, j)
	return j
}

// release ends one streamer's interest in j.
func (d *downloader) release(j *job) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if j.players > 0 {
		j.players--
	}
	if j.players == 0 && !j.keep {
		j.cancel()
	}
}

// stopAll cancels every download and waits for them to record their state.
func (d *downloader) stopAll() {
	d.mu.Lock()
	for _, j := range d.jobs {
		j.cancel()
	}
	d.mu.Unlock()
	d.wg.Wait()
}

func (d *downloader) run(ctx context.Context, j *job) {
	defer d.wg.Done()
	readyOnce := sync.OnceFunc(func() { close(j.infoReady) })
	defer func() {
		readyOnce()
		d.store.Release(j.key)
		d.mu.Lock()
		delete(d.jobs, j.key)
		d.mu.Unlock()
		close(j.done)
	}()

	prefix := d.store.partialPrefix(j.key)
	if err := os.MkdirAll(d.store.abs(partialDir), 0o755); err != nil {
		j.err = err
		return
	}
	// A leftover in-progress fragment makes the resume fail with HTTP 416.
	removePartialFiles(prefix, true)
	resuming := hasPartFile(prefix)

	args := []string{
		"-f", audioFormat,
		"--no-playlist",
		"--no-simulate",
		"--socket-timeout", "15",
		"--print", "before_dl:%(.{id,title,uploader,duration,ext})j",
		"--print", "after_move:%(.{filepath})j",
		"--progress", "--newline",
		"--progress-template", "download:" + progressPrefix + "%(progress.fragment_index)s %(progress.fragment_count)s %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s",
		"-o", d.store.PartialTemplate(j.key),
	}
	if b := ytdlcookies.ForURL(pageBase); b != "" {
		args = append(args, "--cookies-from-browser", b)
	}
	args = append(args, "--", pageURL(j.meta.Permalink))

	cmd := exec.CommandContext(ctx, ytdlpBin, args...)
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		j.err = err
		return
	}
	if err := cmd.Start(); err != nil {
		j.err = fmt.Errorf("start yt-dlp: %w", err)
		return
	}

	var final string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	d.announce(j)
	var lastPct = -1
	var lastEmit time.Time
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), progressPrefix); ok {
			if pct, ok := parseProgress(rest); ok && pct != lastPct && (pct == 100 || time.Since(lastEmit) >= progressEvery) {
				lastPct, lastEmit = pct, time.Now()
				if d.pinned(j) {
					applog.Status("↓ Pinning %s… %d%%", j.name(), pct)
				}
			}
			continue
		}
		var info downloadInfo
		if json.Unmarshal(sc.Bytes(), &info) != nil {
			continue
		}
		if info.Filepath != "" {
			final = info.Filepath
			continue
		}
		if info.Ext == "" {
			continue
		}
		j.info = info
		if info.Title != "" {
			j.setTitle(info.Uploader, info.Title)
		}
		j.partPath = prefix + info.Ext + ".part"
		readyOnce()
	}
	waitErr := cmd.Wait()

	meta := j.meta
	if j.info.Title != "" {
		meta.Title = j.info.Title
	}
	if j.info.Uploader != "" {
		meta.Artist = j.info.Uploader
	}
	if j.info.Duration > 0 {
		meta.Duration = int(j.info.Duration)
	}

	switch {
	case ctx.Err() != nil:
		// Skipped or shutting down: keep the partial download to resume later.
		j.err = context.Canceled
		_ = d.store.Update(meta)
		_ = d.store.SetPartial(j.key, partSize(prefix))
	case waitErr != nil || final == "":
		msg := lastLine(stderr.String())
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		if msg == "" {
			msg = "no file downloaded"
		}
		j.err = fmt.Errorf("yt-dlp: %s", msg)
		if d.pinned(j) {
			applog.UserWarn("✗ Couldn't download %s: %s", j.name(), msg)
		}
		if resuming {
			// The spec's discard rule: a resume that fails starts over next time.
			applog.Warn("soundcloudx: resume of %s failed, discarding the partial download: %v", j.key, j.err)
			_ = d.store.Update(meta)
			_ = d.store.DiscardPartial(j.key)
		} else {
			_ = d.store.Update(meta)
			_ = d.store.SetPartial(j.key, partSize(prefix))
		}
	default:
		file, err := d.store.Commit(meta, final)
		if err != nil {
			j.err = fmt.Errorf("commit download: %w", err)
			return
		}
		j.file = file
		if d.pinned(j) {
			applog.Status("● Pinned %s", j.name())
		}
	}
}

// progressPrefix marks yt-dlp's progress lines on stdout.
const progressPrefix = "scxprog "

// progressEvery throttles the footer updates of one download.
const progressEvery = 1500 * time.Millisecond

// parseProgress turns a progress line into a percentage. HLS downloads
// count fragments; others count bytes. yt-dlp prints NA for unknown fields.
func parseProgress(s string) (int, bool) {
	f := strings.Fields(s)
	if len(f) != 5 {
		return 0, false
	}
	num := func(v string) float64 {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return n
	}
	idx, count, done := num(f[0]), num(f[1]), num(f[2])
	total := num(f[3])
	if total == 0 {
		total = num(f[4])
	}
	var pct float64
	switch {
	case count > 0:
		pct = idx / count * 100
	case total > 0:
		pct = done / total * 100
	default:
		return 0, false
	}
	return int(min(max(pct, 0), 100)), true
}

// pinned reports whether a pin (a favorite or a sync rule) wants j, which is
// when the footer shows its progress. Downloads that only playback wants stay
// quiet: the ○ marker shows them.
func (d *downloader) pinned(j *job) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return j.keep
}

// announce shows that a pinned download started.
func (d *downloader) announce(j *job) {
	if d.pinned(j) {
		applog.Status("↓ Pinning %s…", j.name())
	}
}

// name is the job's track as the footer shows it.
func (j *job) name() string {
	j.nameMu.Lock()
	defer j.nameMu.Unlock()
	if j.title == "" {
		return displayName(j.meta.Artist, j.meta.Title)
	}
	return displayName(j.artist, j.title)
}

// setTitle records the names the download reported.
func (j *job) setTitle(artist, title string) {
	j.nameMu.Lock()
	j.artist, j.title = artist, title
	j.nameMu.Unlock()
}

// hasPartFile reports whether an interrupted download exists for prefix.
func hasPartFile(prefix string) bool {
	return partSize(prefix) > 0
}

// partSize returns the size of the .part file (not the fragment files) that
// starts with prefix.
func partSize(prefix string) int64 {
	matches, _ := globPart(prefix)
	var n int64
	for _, m := range matches {
		n += fileSize(m)
	}
	return n
}

func globPart(prefix string) ([]string, error) {
	matches, err := filepath.Glob(globEscape(prefix) + "*.part")
	if err != nil {
		return nil, err
	}
	out := matches[:0]
	for _, m := range matches {
		if !strings.Contains(m[len(prefix):], "-Frag") {
			out = append(out, m)
		}
	}
	return out, nil
}

// errNotOffline is returned when a remote track is played without network.
var errNotOffline = errors.New("soundcloudx: track is not on disk and could not be downloaded")
