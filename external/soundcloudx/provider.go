package soundcloudx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopxl/beep/v2"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/playlist"
	"github.com/bjarneo/cliamp/provider"
	"github.com/bjarneo/cliamp/resolve"
)

// Compile-time interface checks.
var (
	_ playlist.Provider              = (*Provider)(nil)
	_ playlist.Refresher             = (*Provider)(nil)
	_ playlist.RefreshablePlaylist   = (*Provider)(nil)
	_ provider.Searcher              = (*Provider)(nil)
	_ provider.CustomStreamer        = (*Provider)(nil)
	_ provider.PlaybackStateReporter = (*Provider)(nil)
	_ provider.TrackFavoriter        = (*Provider)(nil)
	_ provider.Closer                = (*Provider)(nil)
	_ provider.ArtistFavoriter       = (*Provider)(nil)
)

// Playlist IDs of the fixed rows.
const (
	listLikes   = "likes"
	listTracks  = "tracks"
	listReposts = "reposts"
	listOnDisk  = "offline"
	listPinned  = "offline:pinned"
	listCached  = "offline:cached"
	artistList  = "artist:"
)

// Markers drawn in the playlist's state column: Nerd Font (Font Awesome)
// icons, the same size as the fork's heart.
const (
	markerPinned  = "\uF111" // nf-fa-circle
	markerCached  = "\uF042" // nf-fa-adjust (half-filled circle)
	markerPartial = "\uF017" // nf-fa-clock_o: waiting to resume
)

const (
	// startTimeout bounds the wait for a download to report its format.
	startTimeout = 45 * time.Second
	// seekBytesPerSec estimates how much audio a byte count of the .part
	// file holds when seeking ahead during a download. It assumes the
	// 160 kbps stream, so it never overestimates.
	seekBytesPerSec = 160_000 / 8
)

// Provider is the SoundCloud provider that caches and pins tracks.
type Provider struct {
	cfg   Config
	store *Store
	dl    *downloader
	list  lister

	// SaveArtists persists the Artists list when c changes it. nil keeps
	// the change for this run only.
	SaveArtists func(artists []string) error

	artistsMu sync.Mutex
	artists   []string

	fetchDurations durationFetcher
	durations      durationState

	syncNow  chan struct{}
	stop     context.CancelFunc
	life     context.Context
	loopDone chan struct{}
	syncMu   sync.Mutex // one sync at a time
}

// NewFromConfig returns the provider, or nil when it is not enabled or its
// storage cannot be opened (the reason is logged).
func NewFromConfig(cfg Config) *Provider {
	if !cfg.IsSet() {
		return nil
	}
	root, err := cfg.root()
	if err != nil {
		applog.UserWarn("soundcloudx: %v", err)
		return nil
	}
	store, err := openStore(root, cfg.CacheMax)
	if err != nil {
		applog.UserWarn("soundcloudx: %v", err)
		return nil
	}
	resolve.SetYTDLCookiesForHost("soundcloud.com", cfg.CookiesFrom)
	p := newProvider(cfg, store, ytdlpList)
	p.fetchDurations = ytdlpDurations
	if len(cfg.Sync) > 0 && cfg.SyncInterval >= 0 {
		p.startSyncLoop()
	}
	return p
}

func newProvider(cfg Config, store *Store, list lister) *Provider {
	cfg.User = strings.TrimSpace(cfg.User)
	life, stop := context.WithCancel(context.Background())
	var artists []string
	for _, a := range cfg.Artists {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(artists, a) {
			artists = append(artists, a)
		}
	}
	return &Provider{
		artists: artists,
		cfg:     cfg,
		store:   store,
		dl:      newDownloader(store),
		list:    list,
		syncNow: make(chan struct{}, 1),
		life:    life,
		stop:    stop,
	}
}

func (p *Provider) Name() string { return "SoundCloud+" }

// Playlists lists the account's pages, the configured artists and the
// offline lists, whose names carry the cache stats.
func (p *Provider) Playlists() ([]playlist.PlaylistInfo, error) {
	var out []playlist.PlaylistInfo
	if p.cfg.User != "" {
		out = append(out,
			playlist.PlaylistInfo{ID: listLikes, Name: "Likes", Section: p.cfg.User},
			playlist.PlaylistInfo{ID: listTracks, Name: "Tracks", Section: p.cfg.User},
			playlist.PlaylistInfo{ID: listReposts, Name: "Reposts", Section: p.cfg.User},
		)
	}
	for _, a := range p.artistList() {
		out = append(out, playlist.PlaylistInfo{ID: artistList + a, Name: p.artistName(a), Section: "Artists"})
	}
	st := p.store.Stats()
	out = append(out,
		playlist.PlaylistInfo{ID: listOnDisk, Name: "All on disk", Section: "Offline", TrackCount: st.PinnedTracks + st.CacheTracks},
		playlist.PlaylistInfo{ID: listPinned, Name: fmt.Sprintf("Pinned · %s", FormatSize(st.PinnedBytes)), Section: "Offline", TrackCount: st.PinnedTracks},
		playlist.PlaylistInfo{ID: listCached, Name: fmt.Sprintf("Cached · %s / %s", FormatSize(st.CacheBytes), FormatSize(st.CacheMax)), Section: "Offline", TrackCount: st.CacheTracks},
	)
	return out, nil
}

// pageFor returns the SoundCloud page a list ID shows, or "" for the
// offline lists.
func (p *Provider) pageFor(id string) string {
	switch id {
	case listLikes, listTracks, listReposts:
		if p.cfg.User == "" {
			return ""
		}
		return pageBase + url.PathEscape(p.cfg.User) + "/" + id
	}
	if a, ok := strings.CutPrefix(id, artistList); ok && a != "" {
		return pageBase + url.PathEscape(a) + "/tracks"
	}
	return ""
}

// Tracks lists a page through yt-dlp. Without network it falls back to the
// last listing stored in the index, where tracks that are not on disk are
// unplayable.
func (p *Provider) Tracks(id string) ([]playlist.Track, error) {
	switch id {
	case listOnDisk, listPinned, listCached:
		return p.offlineTracks(id), nil
	}
	page := p.pageFor(id)
	if page == "" {
		return nil, fmt.Errorf("soundcloudx: unknown list %q", id)
	}
	metas, err := p.list(p.life, page)
	if err != nil {
		keys, ok := p.store.List(id)
		if !ok {
			return nil, err
		}
		applog.UserWarn("soundcloudx: showing the saved list, %v", err)
		return p.tracksFor(keys, true), nil
	}
	keys := make([]string, len(metas))
	for i, m := range metas {
		keys[i] = m.Key
	}
	if err := p.store.Update(metas...); err != nil {
		applog.Warn("soundcloudx: save index: %v", err)
	}
	if err := p.store.SetList(id, keys); err != nil {
		applog.Warn("soundcloudx: save list: %v", err)
	}
	p.fillDurations(keys)
	return p.tracksFor(keys, false), nil
}

// CanRefreshPlaylist lets Ctrl+R reload an open list in place, which shows
// durations fetched since it opened. Implements playlist.RefreshablePlaylist.
func (p *Provider) CanRefreshPlaylist(id string) bool {
	switch id {
	case listOnDisk, listPinned, listCached:
		return true
	}
	return p.pageFor(id) != ""
}

func (p *Provider) offlineTracks(id string) []playlist.Track {
	var out []playlist.Track
	for _, e := range p.store.OnDisk() {
		switch {
		case id == listPinned && e.State() != StatePinned,
			id == listCached && e.State() != StateCached:
			continue
		}
		out = append(out, p.track(e, false))
	}
	return out
}

func (p *Provider) tracksFor(keys []string, offline bool) []playlist.Track {
	out := make([]playlist.Track, 0, len(keys))
	for _, k := range keys {
		if e, ok := p.store.Get(k); ok {
			out = append(out, p.track(e, offline))
		}
	}
	return out
}

// track builds the playlist track for an index entry. Offline, a track
// without a complete file is unplayable, so the UI greys it out and skips it.
func (p *Provider) track(e Entry, offline bool) playlist.Track {
	artist := e.Artist
	if slug := artistFromPermalink(e.Permalink); artist == "" || artist == slug {
		artist = p.artistName(slug)
	}
	title := cleanTitle(e.Title, artist)
	if title == "" {
		title = e.Permalink
	}
	return playlist.Track{
		Path:         buildURI(e.Key, e.Permalink),
		Title:        title,
		Artist:       artist,
		DurationSecs: e.Duration,
		// Stream must be false: true makes pause stop and restart the track.
		Stream:     false,
		Unplayable: offline && e.File == "",
		ProviderMeta: map[string]string{
			MetaID:                   e.Key,
			provider.MetaDisplayName: displayName(artist, title),
		},
	}
}

// artistList returns the Artists section, in the order it was added.
func (p *Provider) artistList() []string {
	p.artistsMu.Lock()
	defer p.artistsMu.Unlock()
	return slices.Clone(p.artists)
}

// artistName returns the display name of an artist slug: the uploader name
// a listing or download reported, or the slug itself.
func (p *Provider) artistName(slug string) string {
	if name := p.store.ArtistName(slug); name != "" {
		return name
	}
	return slug
}

// artistSlug returns the uploader slug of a SoundCloud+ track.
func artistSlug(track playlist.Track) string {
	_, perm, ok := parseURI(track.Path)
	if !ok {
		return ""
	}
	return artistFromPermalink(perm)
}

// CanFavoriteArtist implements provider.ArtistFavoriter.
func (p *Provider) CanFavoriteArtist(track playlist.Track) bool { return artistSlug(track) != "" }

// ArtistFavorited implements provider.ArtistFavoriter.
func (p *Provider) ArtistFavorited(track playlist.Track) bool {
	slug := artistSlug(track)
	p.artistsMu.Lock()
	defer p.artistsMu.Unlock()
	return slug != "" && slices.Contains(p.artists, slug)
}

// ToggleArtistFavorite adds track's uploader to the Artists section, or
// removes it. It never downloads anything: pinning an artist's catalogue is
// the artist:<name> sync rule. Implements provider.ArtistFavoriter.
func (p *Provider) ToggleArtistFavorite(track playlist.Track) (bool, string, error) {
	slug := artistSlug(track)
	if slug == "" {
		return false, "", fmt.Errorf("soundcloudx: no artist for %s", track.Path)
	}
	name := p.artistName(slug)
	if name == slug && track.Artist != "" {
		name = track.Artist
	}

	p.artistsMu.Lock()
	old := slices.Clone(p.artists)
	added := !slices.Contains(p.artists, slug)
	if added {
		p.artists = append(p.artists, slug)
	} else {
		p.artists = slices.DeleteFunc(p.artists, func(a string) bool { return a == slug })
	}
	next := slices.Clone(p.artists)
	p.artistsMu.Unlock()

	if p.SaveArtists != nil {
		if err := p.SaveArtists(next); err != nil {
			p.artistsMu.Lock()
			p.artists = old
			p.artistsMu.Unlock()
			return !added, name, fmt.Errorf("save artists: %w", err)
		}
	}
	return added, name, nil
}

// SearchTracks searches SoundCloud through yt-dlp's scsearch.
func (p *Provider) SearchTracks(ctx context.Context, query string, limit int) ([]playlist.Track, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	metas, err := p.list(ctx, fmt.Sprintf("scsearch%d:%s", limit, q))
	if err != nil {
		return nil, err
	}
	if err := p.store.Update(metas...); err != nil {
		applog.Warn("soundcloudx: save index: %v", err)
	}
	keys := make([]string, len(metas))
	for i, m := range metas {
		keys[i] = m.Key
	}
	return p.tracksFor(keys, false), nil
}

// Refresh runs the sync rules now, besides dropping the UI's list cache.
func (p *Provider) Refresh() {
	select {
	case p.syncNow <- struct{}{}:
	default:
	}
}

// URISchemes implements provider.CustomStreamer.
func (p *Provider) URISchemes() []string { return []string{URIScheme} }

// NewStreamer plays a track from its file when it is on disk. Otherwise it
// starts (or resumes, or joins) the download and plays the file while
// yt-dlp writes it. Implements provider.CustomStreamer.
func (p *Provider) NewStreamer(uri string) (beep.StreamSeekCloser, beep.Format, time.Duration, error) {
	key, perm, ok := parseURI(uri)
	if !ok {
		return nil, beep.Format{}, 0, fmt.Errorf("soundcloudx: invalid URI %q", uri)
	}
	e, known := p.store.Get(key)
	if perm == "" {
		perm = e.Permalink
	}
	meta := Meta{Key: key, Permalink: perm, Title: e.Title, Artist: e.Artist, Duration: e.Duration}
	if !known {
		_ = p.store.Update(meta)
	}

	if path := p.store.FilePath(key); path != "" {
		if _, err := os.Stat(path); err == nil {
			return p.localStreamer(key, path, time.Duration(e.Duration)*time.Second)
		}
		// Deleted outside cliamp: forget it and download again.
		_ = p.store.Forget(key)
	}
	if perm == "" {
		return nil, beep.Format{}, 0, fmt.Errorf("soundcloudx: no page URL for %s", key)
	}
	return p.downloadStreamer(meta)
}

func (p *Provider) localStreamer(key, path string, dur time.Duration) (beep.StreamSeekCloser, beep.Format, time.Duration, error) {
	p.store.Hold(key)
	open := func(off time.Duration) (*decodeProc, error) {
		// The file may have moved between cache/ and pinned/ since it opened.
		if cur := p.store.FilePath(key); cur != "" {
			path = cur
		}
		return startDecode(path, nil, off)
	}
	s, err := newPCMStreamer(open, dur, nil, func() { p.store.Release(key) })
	if err != nil {
		p.store.Release(key)
		return nil, beep.Format{}, 0, err
	}
	_ = p.store.MarkPlayed(key)
	return s, pcmFormat, dur, nil
}

func (p *Provider) downloadStreamer(meta Meta) (beep.StreamSeekCloser, beep.Format, time.Duration, error) {
	j := p.dl.play(meta)
	select {
	case <-j.infoReady:
	case <-time.After(startTimeout):
		p.dl.release(j)
		return nil, beep.Format{}, 0, fmt.Errorf("soundcloudx: download of %s did not start", meta.Key)
	}
	if j.partPath == "" {
		// The job ended before it reported a format.
		<-j.done
		p.dl.release(j)
		if path := p.store.FilePath(meta.Key); path != "" {
			e, _ := p.store.Get(meta.Key)
			return p.localStreamer(meta.Key, path, time.Duration(e.Duration)*time.Second)
		}
		if j.err != nil {
			return nil, beep.Format{}, 0, fmt.Errorf("%w: %v", errNotOffline, j.err)
		}
		return nil, beep.Format{}, 0, errNotOffline
	}

	dur := time.Duration(meta.Duration) * time.Second
	if info := j.Info(); info.Duration > 0 {
		dur = time.Duration(info.Duration * float64(time.Second))
	}
	prefix := p.store.partialPrefix(meta.Key)
	stop := make(chan struct{})
	open := func(off time.Duration) (*decodeProc, error) {
		if path := p.store.FilePath(meta.Key); path != "" {
			return startDecode(path, nil, off)
		}
		if off > 0 {
			avail := time.Duration(partSize(prefix)/seekBytesPerSec) * time.Second
			if off > avail-2*time.Second {
				return nil, fmt.Errorf("soundcloudx: only %v downloaded so far", avail.Truncate(time.Second))
			}
		}
		tail, err := openTail(j.partPath, j.done, stop)
		if errors.Is(err, fs.ErrNotExist) {
			// Finished before the .part file could be opened.
			if path := p.store.FilePath(meta.Key); path != "" {
				return startDecode(path, nil, off)
			}
			if j.err != nil {
				return nil, j.err
			}
			return nil, errNotOffline
		}
		if err != nil {
			return nil, err
		}
		return startDecode("pipe:0", tail, off)
	}
	errFn := func() error {
		select {
		case <-j.done:
			if j.err != nil && !errors.Is(j.err, context.Canceled) {
				return j.err
			}
		default:
		}
		return nil
	}
	release := sync.OnceFunc(func() {
		close(stop)
		p.dl.release(j)
	})
	s, err := newPCMStreamer(open, dur, errFn, release)
	if err != nil {
		release()
		return nil, beep.Format{}, 0, err
	}
	_ = p.store.MarkPlayed(meta.Key)
	return s, pcmFormat, dur, nil
}

// HasPlaybackState implements provider.PlaybackStateReporter.
func (p *Provider) HasPlaybackState() bool { return p.store.HasAny() }

// PlaybackState reports the pinned, cached or partial marker of a track
// from memory. Implements provider.PlaybackStateReporter.
func (p *Provider) PlaybackState(track playlist.Track) (provider.PlaybackState, bool) {
	key, _, ok := parseURI(track.Path)
	if !ok {
		return provider.PlaybackState{}, false
	}
	e, ok := p.store.Get(key)
	if !ok {
		return provider.PlaybackState{}, false
	}
	switch e.State() {
	case StatePinned:
		return provider.PlaybackState{Played: true, Marker: markerPinned}, true
	case StateCached:
		return provider.PlaybackState{Marker: markerCached}, true
	case StatePartial:
		return provider.PlaybackState{Marker: markerPartial}, true
	}
	return provider.PlaybackState{}, false
}

// CanFavoriteTrack implements provider.TrackFavoriter. A favorite is a
// manual pin.
func (p *Provider) CanFavoriteTrack(track playlist.Track) bool {
	_, _, ok := parseURI(track.Path)
	return ok
}

// SetTrackFavorite pins a track by hand (and downloads it in the
// background) or lets the manual pin go. Implements provider.TrackFavoriter.
func (p *Provider) SetTrackFavorite(_ context.Context, track playlist.Track, favorite bool) error {
	key, perm, ok := parseURI(track.Path)
	if !ok {
		return fmt.Errorf("soundcloudx: not a SoundCloud+ track: %s", track.Path)
	}
	if !favorite {
		return p.store.RemoveSource(key, SourceManual)
	}
	meta := Meta{Key: key, Permalink: perm, Title: track.Title, Artist: track.Artist, Duration: track.DurationSecs}
	missing, err := p.store.AddSource(meta, SourceManual)
	if err != nil {
		return err
	}
	if missing {
		p.dl.pin(meta)
	}
	return nil
}

// Close stops the sync loop and the downloads. Interrupted downloads stay
// on disk to resume later. Implements provider.Closer.
func (p *Provider) Close() {
	p.stop()
	if p.loopDone != nil {
		<-p.loopDone
	}
	p.dl.stopAll()
}
