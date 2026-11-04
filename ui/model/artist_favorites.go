package model

import (
	tea "charm.land/bubbletea/v2"

	"github.com/bjarneo/cliamp/playlist"
	"github.com/bjarneo/cliamp/provider"
)

// findArtistFavoriter returns the provider that can favorite track's
// artist, and the provider's name.
func (m *Model) findArtistFavoriter(track playlist.Track) (provider.ArtistFavoriter, string) {
	return findCapable(m, func(f provider.ArtistFavoriter) bool { return f.CanFavoriteArtist(track) })
}

// canFavoriteArtist reports whether c applies to track. It does no I/O.
func (m Model) canFavoriteArtist(track playlist.Track) bool {
	f, _ := m.findArtistFavoriter(track)
	return f != nil
}

// artistFavoriteLabel names the c action for track in the help bar.
func artistFavoriteLabel(m Model, track playlist.Track) string {
	if f, _ := m.findArtistFavoriter(track); f != nil && f.ArtistFavorited(track) {
		return "Unfavorite artist"
	}
	return "Favorite artist"
}

// favoriteArtistKey toggles the favorite of track's artist. A provider pane
// that shows the artist list reloads so the change appears at once.
func (m *Model) favoriteArtistKey(track playlist.Track) tea.Cmd {
	f, provName := m.findArtistFavoriter(track)
	if f == nil {
		return nil
	}
	favorite, name, err := f.ToggleArtistFavorite(track)
	if err != nil {
		m.status.Errorf(statusTTLDefault, "Favorite artist failed: %s", err)
		return nil
	}
	if favorite {
		m.status.Showf(statusTTLMedium, "%s Added %s to Artists", favHeart, name)
	} else {
		m.status.Showf(statusTTLMedium, "Removed %s from Artists", name)
	}
	if m.isActiveProvider(provName) {
		return m.fetchProviderPlaylists()
	}
	return nil
}

// selectedPlaylistTrack returns the track under the playlist cursor.
func (m Model) selectedPlaylistTrack() (playlist.Track, bool) {
	if m.playlist == nil || m.focus != focusPlaylist || m.plCursor < 0 || m.plCursor >= m.playlist.Len() {
		return playlist.Track{}, false
	}
	return m.playlist.Track(m.plCursor)
}

// selectedSearchResult returns the track result under the search cursor.
func (m Model) selectedSearchResult() (playlist.Track, bool) {
	if m.searchOverlay.screen != searchOverlayResults || m.searchOverlay.cursor < 0 || m.searchOverlay.cursor >= len(m.searchOverlay.results) {
		return playlist.Track{}, false
	}
	t := m.searchOverlay.results[m.searchOverlay.cursor]
	return t, !t.IsAlbum()
}
