package soundcloudx

import (
	"errors"
	"slices"
	"testing"

	"github.com/bjarneo/cliamp/playlist"
	"github.com/bjarneo/cliamp/provider"
)

func TestCleanTitle(t *testing.T) {
	for _, tt := range []struct{ title, artist, want string }{
		{"Forss - Flickermood", "Forss", "Flickermood"},
		{"forss – Flickermood", "Forss", "Flickermood"},
		{"Flickermood", "Forss", "Flickermood"},
		{"DJ Pulse - Holding Me (Robert Forss Remix)", "Robert Forss Playlists", "DJ Pulse - Holding Me (Robert Forss Remix)"},
		{"Forss - ", "Forss", "Forss -"},
		{"Forssman - Song", "Forss", "Forssman - Song"},
		{"Song", "", "Song"},
	} {
		if got := cleanTitle(tt.title, tt.artist); got != tt.want {
			t.Errorf("cleanTitle(%q, %q) = %q, want %q", tt.title, tt.artist, got, tt.want)
		}
	}
	if got := displayName("Forss", "Forss - Flickermood"); got != "Forss – Flickermood" {
		t.Errorf("displayName = %q", got)
	}
}

func TestParseProgress(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"3 7 1000 NA 5000", 42, true},   // HLS: fragments
		{"NA NA 250 1000 NA", 25, true},  // bytes
		{"NA NA 250 NA 500.5", 49, true}, // estimate
		{"7 7 9000 NA 9000", 100, true},  //
		{"NA NA 10 NA NA", 0, false},     // nothing to divide by
		{"garbage", 0, false},            //
	} {
		got, ok := parseProgress(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseProgress(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestToggleArtistFavoriteSavesAndRollsBack(t *testing.T) {
	p := newProvider(Config{Artists: []string{"forss"}}, newTestStore(t, 0), nil)
	var saved []string
	p.SaveArtists = func(a []string) error { saved = a; return nil }
	_ = p.store.Update(Meta{Key: "1", Permalink: "other/song", Title: "Song", Artist: "Other Name"})
	track := p.track(mustGet(t, p.store, "1"), false)

	if p.ArtistFavorited(track) {
		t.Fatal("not a favorite yet")
	}
	fav, name, err := p.ToggleArtistFavorite(track)
	if err != nil || !fav || name != "Other Name" || !slices.Equal(saved, []string{"forss", "other"}) {
		t.Fatalf("add = %v %q %v, saved %v", fav, name, err, saved)
	}
	lists, _ := p.Playlists()
	if !slices.ContainsFunc(lists, func(pl playlist.PlaylistInfo) bool { return pl.ID == "artist:other" && pl.Name == "Other Name" }) {
		t.Fatalf("Artists section = %+v", lists)
	}

	p.SaveArtists = func([]string) error { return errors.New("disk full") }
	if _, _, err := p.ToggleArtistFavorite(track); err == nil {
		t.Fatal("want the save error")
	}
	if !p.ArtistFavorited(track) {
		t.Fatal("a failed save must keep the artist")
	}
}

func TestTrackUsesKnownArtistNameAndDisplay(t *testing.T) {
	p := newProvider(Config{}, newTestStore(t, 0), nil)
	// An artist page only knows the slug; a search taught the real name.
	_ = p.store.Update(
		Meta{Key: "1", Permalink: "forss/flickermood", Title: "Forss - Flickermood", Artist: "forss"},
		Meta{Key: "2", Permalink: "forss/veniam", Title: "Veniam", Artist: "Forss"},
	)
	tr := p.track(mustGet(t, p.store, "1"), false)
	if tr.Artist != "Forss" || tr.Title != "Flickermood" || tr.Meta(provider.MetaDisplayName) != "Forss – Flickermood" {
		t.Fatalf("track = %+v", tr)
	}
}

func mustGet(t *testing.T, s *Store, key string) Entry {
	t.Helper()
	e, ok := s.Get(key)
	if !ok {
		t.Fatalf("no entry %s", key)
	}
	return e
}

func TestOpenStoreLearnsNamesFromOldEntries(t *testing.T) {
	s := newTestStore(t, 0)
	// Written as an older build did: real names in entries, no names table.
	s.tracks["1"] = &Entry{Key: "1", Permalink: "bastombcn/home-session-15", Artist: "Bastom"}
	s.tracks["2"] = &Entry{Key: "2", Permalink: "bastombcn/home-session-11", Artist: "bastombcn"}
	s.mu.Lock()
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()

	again, err := openStore(s.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider(Config{}, again, nil)
	if tr := p.track(mustGet(t, again, "2"), false); tr.Artist != "Bastom" {
		t.Fatalf("slug-only track artist = %q, want Bastom", tr.Artist)
	}
}
