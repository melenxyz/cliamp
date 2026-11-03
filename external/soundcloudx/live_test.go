package soundcloudx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveSoundCloud runs the real yt-dlp and ffmpeg against SoundCloud.
// It is off by default: SOUNDCLOUDX_LIVE=1 go test -run Live -v ./external/soundcloudx/
func TestLiveSoundCloud(t *testing.T) {
	if os.Getenv("SOUNDCLOUDX_LIVE") == "" {
		t.Skip("set SOUNDCLOUDX_LIVE=1 to run against soundcloud.com")
	}
	store, err := openStore(filepath.Join(t.TempDir(), "store"), 0)
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider(Config{Artists: []string{"forss"}}, store, ytdlpList)
	t.Cleanup(p.Close)

	tracks, err := p.Tracks("artist:forss")
	if err != nil || len(tracks) == 0 {
		t.Fatalf("Tracks = %d, %v", len(tracks), err)
	}
	var uri string
	for _, tr := range tracks {
		if tr.Title == "Veniam" {
			uri = tr.Path
		}
	}
	if uri == "" {
		uri = tracks[0].Path
	}
	t.Logf("playing %s", uri)

	// Skip early: a partial download is kept.
	s, _, dur, err := p.NewStreamer(uri)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Stream(make([][2]float64, 44100))
	t.Logf("first second of audio after %v, duration %v", time.Since(start), dur)
	s.Close()
	key, _, _ := parseURI(uri)
	waitFor(t, func() bool { e, _ := p.store.Get(key); return e.State() != StateRemote })
	e, _ := p.store.Get(key)
	t.Logf("after skip: state %v, partial %d, file %q", e.State(), e.PartialSize, e.File)

	// Play again: resumes (or plays from disk) and streams to the end.
	s, _, _, err = p.NewStreamer(uri)
	if err != nil {
		t.Fatal(err)
	}
	frames := drain(t, s)
	s.Close()
	t.Logf("streamed %.1fs", float64(frames)/sampleRate)
	if got := time.Duration(float64(frames) / sampleRate * float64(time.Second)); dur > 0 && got < dur-2*time.Second {
		t.Fatalf("streamed %v of %v", got, dur)
	}
	waitFor(t, func() bool { e, _ := p.store.Get(key); return e.State() == StateCached })
	e, _ = p.store.Get(key)
	t.Logf("committed: %+v", e)

	// Pin it: the file moves to pinned/ with a readable name.
	if _, err := p.store.AddSource(Meta{Key: key}, SourceManual); err != nil {
		t.Fatal(err)
	}
	e, _ = p.store.Get(key)
	t.Logf("pinned file: %s", e.File)

	if _, err := p.SearchTracks(context.Background(), "forss flickermood", 3); err != nil {
		t.Fatal(err)
	}
}
