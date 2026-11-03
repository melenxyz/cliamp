package soundcloudx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, cacheMax int64) *Store {
	t.Helper()
	s, err := openStore(t.TempDir(), cacheMax)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// download writes a fake finished download of size bytes for key and
// returns its path, as yt-dlp would leave it in partial/.
func download(t *testing.T, s *Store, key string, size int) string {
	t.Helper()
	path := s.partialPrefix(key) + "m4a"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func meta(key string) Meta {
	return Meta{Key: key, Permalink: "artist/" + key, Title: "Title " + key, Artist: "Artist"}
}

func TestStoreCommitToCacheAndPinned(t *testing.T) {
	s := newTestStore(t, 1<<20)
	if _, err := s.Commit(meta("1"), download(t, s, "1", 100)); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get("1")
	if e.State() != StateCached || e.File != "cache/1.m4a" || e.Size != 100 {
		t.Fatalf("cached entry = %+v", e)
	}

	if _, err := s.AddSource(meta("2"), "likes"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(meta("2"), download(t, s, "2", 50)); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Get("2")
	if e.State() != StatePinned || e.File != "pinned/Artist/Title 2 [2].m4a" {
		t.Fatalf("pinned entry = %+v", e)
	}
	if _, err := os.Stat(s.abs(e.File)); err != nil {
		t.Fatal(err)
	}
}

func TestStorePinMovesCachedFileAndDemoteKeepsLastPlayed(t *testing.T) {
	s := newTestStore(t, 1<<20)
	s.now = func() time.Time { return time.Unix(1000, 0) }
	if _, err := s.Commit(meta("1"), download(t, s, "1", 10)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPlayed("1"); err != nil {
		t.Fatal(err)
	}

	missing, err := s.AddSource(meta("1"), "likes")
	if err != nil || missing {
		t.Fatalf("AddSource = %v, %v; want no download", missing, err)
	}
	if _, err := s.AddSource(meta("1"), SourceManual); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get("1")
	if e.State() != StatePinned || !strings.HasPrefix(e.File, "pinned/") {
		t.Fatalf("after pin: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(s.root, "cache", "1.m4a")); !os.IsNotExist(err) {
		t.Fatal("cached copy must be moved, not copied")
	}

	// One source letting go keeps it pinned.
	if err := s.RemoveSource("1", "likes"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get("1"); e.State() != StatePinned {
		t.Fatalf("still held by manual: %+v", e)
	}
	// The last one demotes it to the cache.
	if err := s.RemoveSource("1", SourceManual); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Get("1")
	if e.State() != StateCached || e.File != "cache/1.m4a" || e.LastPlayed != 1000 {
		t.Fatalf("after demote: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(s.root, "pinned", "Artist")); !os.IsNotExist(err) {
		t.Fatal("empty artist directory should be removed")
	}
}

func TestStoreEvictsLeastRecentlyPlayedAndSkipsPinnedAndBusy(t *testing.T) {
	s := newTestStore(t, 250)
	clock := int64(0)
	s.now = func() time.Time { clock++; return time.Unix(clock, 0) }

	// Pinned tracks never count toward the cache.
	if _, err := s.AddSource(meta("pin"), "likes"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(meta("pin"), download(t, s, "pin", 1000)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b"} {
		if _, err := s.Commit(meta(k), download(t, s, k, 100)); err != nil {
			t.Fatal(err)
		}
	}
	// Play order: b, then a. So b is the least recently played.
	_ = s.MarkPlayed("b")
	_ = s.MarkPlayed("a")

	s.Hold("c") // c is playing: never evicted while held
	if _, err := s.Commit(meta("c"), download(t, s, "c", 100)); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get("b"); e.State() != StateRemote {
		t.Fatalf("b should be evicted first: %+v", e)
	}
	for _, k := range []string{"a", "c", "pin"} {
		if e, _ := s.Get(k); e.File == "" {
			t.Fatalf("%s should stay: %+v", k, e)
		}
	}
	st := s.Stats()
	if st.CacheBytes != 200 || st.CacheTracks != 2 || st.PinnedBytes != 1000 || st.PinnedTracks != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestStorePartialCountsTowardCacheAndIsEvicted(t *testing.T) {
	s := newTestStore(t, 150)
	if err := s.Update(meta("p")); err != nil {
		t.Fatal(err)
	}
	part := s.partialPrefix("p") + "m4a.part"
	_ = os.MkdirAll(filepath.Dir(part), 0o755)
	_ = os.WriteFile(part, make([]byte, 100), 0o644)
	if err := s.SetPartial("p", 100); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get("p"); e.State() != StatePartial {
		t.Fatalf("partial state: %+v", e)
	}
	if _, err := s.Commit(meta("x"), download(t, s, "x", 100)); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get("p"); e.PartialSize != 0 {
		t.Fatalf("partial should be evicted: %+v", e)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("partial file should be deleted")
	}
}

func TestStorePersists(t *testing.T) {
	s := newTestStore(t, 1<<20)
	if _, err := s.AddSource(meta("1"), "artist:x"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetList("artist:x", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	again, err := openStore(s.root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := again.Get("1")
	if !ok || !slices.Equal(e.Sources, []string{"artist:x"}) || e.Title != "Title 1" {
		t.Fatalf("reloaded entry = %+v", e)
	}
	if keys, ok := again.List("artist:x"); !ok || !slices.Equal(keys, []string{"1"}) {
		t.Fatalf("reloaded list = %v", keys)
	}
}

func TestRemovePartialFilesFragsOnly(t *testing.T) {
	s := newTestStore(t, 0)
	prefix := s.partialPrefix("k")
	_ = os.MkdirAll(filepath.Dir(prefix), 0o755)
	for _, name := range []string{"m4a.part", "m4a.ytdl", "m4a.part-Frag3.part"} {
		_ = os.WriteFile(prefix+name, []byte("x"), 0o644)
	}
	removePartialFiles(prefix, true)
	for name, want := range map[string]bool{"m4a.part": true, "m4a.ytdl": true, "m4a.part-Frag3.part": false} {
		_, err := os.Stat(prefix + name)
		if (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
	if got := partSize(prefix); got != 1 {
		t.Errorf("partSize = %d, want 1 (the fragment does not count)", got)
	}
}
