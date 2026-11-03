package soundcloudx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bjarneo/cliamp/playlist"
)

// fakeYTDLP is a yt-dlp stand-in. It prints the before_dl JSON, writes a
// fragmented MP4 to <out>.part in chunks (CHUNK_DELAY seconds apart), renames
// it like yt-dlp does, and prints the after_move JSON. FAKE_FAIL=1 makes it
// fail before writing anything.
const fakeYTDLP = `#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    --) url="$2"; shift ;;
  esac
  shift
done
if [ "$FAKE_FAIL" = "1" ]; then echo "ERROR: fake failure for $url" >&2; exit 1; fi
out=$(printf '%s' "$out" | sed 's/%(ext)s/m4a/')
id=$(basename "$url")
echo "{\"id\": \"$id\", \"title\": \"Song $id\", \"uploader\": \"Real Name\", \"duration\": 3.0, \"ext\": \"m4a\"}"
split -b 8192 "$FAKE_AUDIO" "$out.chunk."
for c in "$out".chunk.*; do
  cat "$c" >> "$out.part"; rm -f "$c"
  sleep "${CHUNK_DELAY:-0}"
done
mv "$out.part" "$out"
echo "{\"filepath\": \"$out\"}"
`

// setupFakes installs the fake yt-dlp and a 3-second fragmented-MP4 test
// tone, and returns a provider over a temp store.
func setupFakes(t *testing.T, list lister, cfg Config) *Provider {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	audio := filepath.Join(dir, "tone.m4a")
	gen := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:a", "aac", "-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-frag_duration", "500000", audio)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("make test tone: %v: %s", err, out)
	}
	script := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(script, []byte(fakeYTDLP), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin := ytdlpBin
	ytdlpBin = script
	t.Cleanup(func() { ytdlpBin = oldBin })
	t.Setenv("FAKE_AUDIO", audio)

	store, err := openStore(filepath.Join(dir, "store"), cfg.CacheMax)
	if err != nil {
		t.Fatal(err)
	}
	if list == nil {
		list = func(context.Context, string) ([]Meta, error) { return nil, errors.New("offline") }
	}
	p := newProvider(cfg, store, list)
	t.Cleanup(p.Close)
	return p
}

// drain streams s to the end and returns the number of frames.
func drain(t *testing.T, s interface {
	Stream([][2]float64) (int, bool)
}) int {
	t.Helper()
	buf := make([][2]float64, 4096)
	total := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		n, ok := s.Stream(buf)
		total += n
		if !ok {
			return total
		}
	}
	t.Fatal("stream did not end")
	return 0
}

func TestStreamAndSaveThenPlayFromDisk(t *testing.T) {
	t.Setenv("CHUNK_DELAY", "0.05")
	p := setupFakes(t, nil, Config{})
	uri := buildURI("42", "artist/42")

	s, format, dur, err := p.NewStreamer(uri)
	if err != nil {
		t.Fatal(err)
	}
	if format.SampleRate != sampleRate || dur != 3*time.Second {
		t.Fatalf("format %+v, duration %v", format, dur)
	}
	frames := drain(t, s)
	if want := 3 * sampleRate; frames < want-sampleRate/10 || frames > want+sampleRate/10 {
		t.Fatalf("streamed %d frames during download, want about %d", frames, want)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// The download commits once yt-dlp exits.
	waitFor(t, func() bool { e, _ := p.store.Get("42"); return e.State() == StateCached })
	e, _ := p.store.Get("42")
	if e.Title != "Song 42" || e.Artist != "Real Name" || e.Duration != 3 || e.LastPlayed == 0 {
		t.Fatalf("index entry = %+v", e)
	}

	// Second play: from disk, and seekable.
	s2, _, _, err := p.NewStreamer(uri)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Seek(2 * sampleRate); err != nil {
		t.Fatal(err)
	}
	if rest := drain(t, s2); rest > sampleRate+sampleRate/10 {
		t.Fatalf("after seeking to 2s, %d frames remain", rest)
	}

	st, ok := p.PlaybackState(playlist.Track{Path: uri})
	if !ok || st.Marker != markerCached {
		t.Fatalf("PlaybackState = %+v, %v", st, ok)
	}
}

func TestSkipEarlyKeepsPartial(t *testing.T) {
	t.Setenv("CHUNK_DELAY", "0.3")
	p := setupFakes(t, nil, Config{})
	s, _, _, err := p.NewStreamer(buildURI("7", "artist/7"))
	if err != nil {
		t.Fatal(err)
	}
	s.Stream(make([][2]float64, 512))
	s.Close()
	waitFor(t, func() bool { e, _ := p.store.Get("7"); return e.State() == StatePartial })
	if st, _ := p.PlaybackState(playlist.Track{Path: buildURI("7", "artist/7")}); st.Marker != markerPartial {
		t.Fatalf("marker = %q", st.Marker)
	}
}

func TestFailedDownloadReportsError(t *testing.T) {
	t.Setenv("FAKE_FAIL", "1")
	p := setupFakes(t, nil, Config{})
	_, _, _, err := p.NewStreamer(buildURI("9", "artist/9"))
	if err == nil || !strings.Contains(err.Error(), "fake failure") {
		t.Fatalf("NewStreamer error = %v", err)
	}
}

// fakeLister returns fixed listings per page and can be switched offline.
type fakeLister struct {
	mu      sync.Mutex
	pages   map[string][]Meta
	offline bool
}

func (f *fakeLister) list(_ context.Context, page string) ([]Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offline {
		return nil, errors.New("network down")
	}
	m, ok := f.pages[page]
	if !ok {
		return nil, fmt.Errorf("no page %s", page)
	}
	return m, nil
}

func (f *fakeLister) set(page string, metas ...Meta) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages[page] = metas
}

func TestSyncPinsDemotesAndSurvivesNetworkErrors(t *testing.T) {
	fl := &fakeLister{pages: map[string][]Meta{}}
	likes := "https://soundcloud.com/me/likes"
	forss := "https://soundcloud.com/forss/tracks"
	fl.set(likes, meta("a"), meta("b"))
	fl.set(forss, meta("b"), meta("c"))
	p := setupFakes(t, fl.list, Config{User: "me", Sync: []string{"likes", "artist:forss"}})

	ctx := context.Background()
	if err := p.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		e, _ := p.store.Get(k)
		if e.State() != StatePinned {
			t.Fatalf("%s after sync: %+v", k, e)
		}
	}
	if e, _ := p.store.Get("b"); len(e.Sources) != 2 {
		t.Fatalf("b should be held by both rules: %v", e.Sources)
	}

	// A failing listing changes nothing.
	fl.offline = true
	if err := p.Sync(ctx); err == nil {
		t.Fatal("want an error from the failed listings")
	}
	if e, _ := p.store.Get("a"); e.State() != StatePinned {
		t.Fatalf("a was demoted by a network error: %+v", e)
	}

	// Unliking a: demoted to the cache, not deleted. b stays pinned by forss.
	fl.offline = false
	fl.set(likes, meta("b"))
	if err := p.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if e, _ := p.store.Get("a"); e.State() != StateCached {
		t.Fatalf("a after unlike: %+v", e)
	}

	// Removing the artist rule lets go of c; b is still liked.
	p.cfg.Sync = []string{"likes"}
	if err := p.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if e, _ := p.store.Get("c"); e.State() != StateCached {
		t.Fatalf("c after rule removal: %+v", e)
	}
	if e, _ := p.store.Get("b"); e.State() != StatePinned {
		t.Fatalf("b after rule removal: %+v", e)
	}
}

func TestTracksFallsBackToSavedListOffline(t *testing.T) {
	fl := &fakeLister{pages: map[string][]Meta{}}
	page := "https://soundcloud.com/forss/tracks"
	fl.set(page, meta("x"), meta("y"))
	p := setupFakes(t, fl.list, Config{Artists: []string{"forss"}})

	if _, err := p.Tracks("artist:forss"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTrackFavorite(context.Background(), playlist.Track{Path: buildURI("x", "artist/x")}, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { e, _ := p.store.Get("x"); return e.State() == StatePinned })

	fl.offline = true
	tracks, err := p.Tracks("artist:forss")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].Unplayable || !tracks[1].Unplayable {
		t.Fatalf("offline tracks = %+v", tracks)
	}

	onDisk, _ := p.Tracks(listOnDisk)
	if len(onDisk) != 1 || onDisk[0].Path != buildURI("x", "artist/x") {
		t.Fatalf("on disk = %+v", onDisk)
	}

	// Unpinning by hand demotes to the cache.
	if err := p.SetTrackFavorite(context.Background(), onDisk[0], false); err != nil {
		t.Fatal(err)
	}
	if e, _ := p.store.Get("x"); e.State() != StateCached {
		t.Fatalf("after unpin: %+v", e)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
