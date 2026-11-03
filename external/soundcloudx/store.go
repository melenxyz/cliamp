package soundcloudx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is where a track lives.
type State int

const (
	StateRemote  State = iota // nothing on disk
	StatePartial              // an interrupted download waits to be resumed
	StateCached               // complete file in cache/, subject to eviction
	StatePinned               // complete file in pinned/, held by a pin source
)

// SourceManual is the pin source of a track pinned by hand.
const SourceManual = "manual"

const (
	indexName  = "index.json"
	cacheDir   = "cache"
	pinnedDir  = "pinned"
	partialDir = "partial"
)

// Entry is one track in the index.
type Entry struct {
	Key         string   `json:"key"`
	Permalink   string   `json:"permalink"`
	Title       string   `json:"title,omitempty"`
	Artist      string   `json:"artist,omitempty"`
	Duration    int      `json:"duration,omitempty"` // seconds
	File        string   `json:"file,omitempty"`     // complete file, relative to the root
	Size        int64    `json:"size,omitempty"`
	PartialSize int64    `json:"partial_size,omitempty"`
	LastPlayed  int64    `json:"last_played,omitempty"` // unix seconds
	Sources     []string `json:"sources,omitempty"`     // pin sources that hold the track
}

// State returns where the entry's audio lives.
func (e Entry) State() State {
	switch {
	case e.File != "" && len(e.Sources) > 0:
		return StatePinned
	case e.File != "":
		return StateCached
	case e.PartialSize > 0:
		return StatePartial
	}
	return StateRemote
}

type indexFile struct {
	Version int                 `json:"version"`
	Tracks  map[string]*Entry   `json:"tracks"`
	Lists   map[string][]string `json:"lists,omitempty"`
}

// Stats summarizes what is on disk.
type Stats struct {
	CacheBytes   int64 // cached files and partial downloads
	CacheTracks  int
	PinnedBytes  int64
	PinnedTracks int
	CacheMax     int64
}

// Store is the local index of tracks and their files. All methods are safe
// for concurrent use. Reads answer from memory; every change is written to
// index.json before the method returns.
type Store struct {
	root     string
	cacheMax int64
	now      func() time.Time

	mu     sync.RWMutex
	tracks map[string]*Entry
	lists  map[string][]string
	busy   map[string]int // keys that eviction must skip: playing or downloading
}

// openStore loads root/index.json. A missing index is an empty store; the
// directory is created on the first write.
func openStore(root string, cacheMax int64) (*Store, error) {
	if cacheMax <= 0 {
		cacheMax = DefaultCacheMax
	}
	s := &Store{
		root:     root,
		cacheMax: cacheMax,
		now:      time.Now,
		tracks:   map[string]*Entry{},
		lists:    map[string][]string{},
		busy:     map[string]int{},
	}
	data, err := os.ReadFile(filepath.Join(root, indexName))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f indexFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("soundcloudx: read %s: %w", indexName, err)
	}
	for k, e := range f.Tracks {
		if e == nil {
			continue
		}
		e.Key = k
		s.tracks[k] = e
	}
	if f.Lists != nil {
		s.lists = f.Lists
	}
	return s, nil
}

// saveLocked writes the index atomically. s.mu must be held.
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(indexFile{Version: 1, Tracks: s.tracks, Lists: s.lists}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.root, indexName+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.root, indexName))
}

// abs returns the absolute path of a root-relative file.
func (s *Store) abs(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

// Get returns a copy of the entry for key.
func (s *Store) Get(key string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tracks[key]
	if !ok {
		return Entry{}, false
	}
	return copyEntry(e), true
}

func copyEntry(e *Entry) Entry {
	c := *e
	c.Sources = slices.Clone(e.Sources)
	return c
}

// FilePath returns the absolute path of key's complete file, or "" when
// there is none.
func (s *Store) FilePath(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.tracks[key]; ok && e.File != "" {
		return s.abs(e.File)
	}
	return ""
}

// Meta is what a listing or a download knows about a track.
type Meta struct {
	Key       string
	Permalink string
	Title     string
	Artist    string
	Duration  int
}

// entryLocked returns the entry for m.Key, creating it, and merges the
// non-empty fields of m into it. s.mu must be held for writing.
func (s *Store) entryLocked(m Meta) *Entry {
	e, ok := s.tracks[m.Key]
	if !ok {
		e = &Entry{Key: m.Key}
		s.tracks[m.Key] = e
	}
	if m.Permalink != "" {
		e.Permalink = m.Permalink
	}
	if m.Title != "" {
		e.Title = m.Title
	}
	// A listing only knows the user slug; keep a real name a download found.
	if m.Artist != "" && (e.Artist == "" || e.Artist == artistFromPermalink(e.Permalink)) {
		e.Artist = m.Artist
	}
	if m.Duration > 0 {
		e.Duration = m.Duration
	}
	return e
}

// Update merges metadata into the index. Entries for remote tracks are kept
// so offline lists can still show them.
func (s *Store) Update(metas ...Meta) error {
	if len(metas) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range metas {
		s.entryLocked(m)
	}
	return s.saveLocked()
}

// MarkPlayed records that key started playing now.
func (s *Store) MarkPlayed(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tracks[key]
	if !ok {
		return nil
	}
	e.LastPlayed = s.now().Unix()
	return s.saveLocked()
}

// Hold protects key from eviction until the matching Release.
func (s *Store) Hold(key string) {
	s.mu.Lock()
	s.busy[key]++
	s.mu.Unlock()
}

// Release ends one Hold of key.
func (s *Store) Release(key string) {
	s.mu.Lock()
	if s.busy[key]--; s.busy[key] <= 0 {
		delete(s.busy, key)
	}
	s.mu.Unlock()
}

// partialPrefix returns the path prefix of key's partial download files.
func (s *Store) partialPrefix(key string) string {
	return filepath.Join(s.root, partialDir, key+".")
}

// PartialTemplate returns the yt-dlp output template for key's download.
func (s *Store) PartialTemplate(key string) string {
	return s.partialPrefix(key) + "%(ext)s"
}

// SetPartial records the size of key's interrupted download, or clears it
// with size 0. Eviction then counts it toward the cache.
func (s *Store) SetPartial(key string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tracks[key]
	if !ok || e.PartialSize == size {
		return nil
	}
	e.PartialSize = size
	if _, err := s.evictLocked(); err != nil {
		return err
	}
	return s.saveLocked()
}

// DiscardPartial deletes key's partial download files and clears the record.
func (s *Store) DiscardPartial(key string) error {
	removePartialFiles(s.partialPrefix(key), false)
	return s.SetPartial(key, 0)
}

// removePartialFiles deletes the partial download files that start with
// prefix. With fragsOnly it deletes only the in-progress fragment files
// (*-Frag*.part), which break a resume with HTTP 416.
func removePartialFiles(prefix string, fragsOnly bool) {
	matches, _ := filepath.Glob(globEscape(prefix) + "*")
	for _, m := range matches {
		if fragsOnly && !strings.Contains(filepath.Base(m), "-Frag") {
			continue
		}
		_ = os.Remove(m)
	}
}

// globEscape escapes the glob metacharacters in a literal path.
func globEscape(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return r.Replace(p)
}

// Commit moves a finished download into the store: to pinned/ when a pin
// source holds the track, to cache/ otherwise. It then evicts as needed.
func (s *Store) Commit(m Meta, downloaded string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(m)
	ext := filepath.Ext(downloaded)
	var rel string
	if len(e.Sources) > 0 {
		rel = pinnedRel(e, ext)
	} else {
		rel = cacheRel(e.Key, ext)
	}
	dst := s.abs(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(downloaded, dst); err != nil {
		return "", err
	}
	if e.File != "" && e.File != rel {
		_ = os.Remove(s.abs(e.File))
	}
	e.File = rel
	e.Size = fileSize(dst)
	e.PartialSize = 0
	removePartialFiles(s.partialPrefix(e.Key), false)
	if _, err := s.evictLocked(); err != nil {
		return "", err
	}
	return dst, s.saveLocked()
}

// Forget clears key's file record when the file was deleted outside cliamp.
func (s *Store) Forget(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tracks[key]
	if !ok || e.File == "" {
		return nil
	}
	e.File, e.Size = "", 0
	return s.saveLocked()
}

func cacheRel(key, ext string) string { return cacheDir + "/" + key + ext }

func pinnedRel(e *Entry, ext string) string {
	artist := sanitizeName(e.Artist)
	if artist == "" {
		artist = sanitizeName(artistFromPermalink(e.Permalink))
	}
	if artist == "" {
		artist = "Unknown"
	}
	title := sanitizeName(e.Title)
	if title == "" {
		title = "Untitled"
	}
	return pinnedDir + "/" + artist + "/" + title + " [" + e.Key + "]" + ext
}

// sanitizeName makes s safe as one path component.
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	s = strings.Trim(s, ". ")
	if len(s) > 150 {
		s = s[:150]
	}
	return s
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// relocateLocked moves e's file to rel. s.mu must be held.
func (s *Store) relocateLocked(e *Entry, rel string) error {
	if e.File == "" || e.File == rel {
		return nil
	}
	dst := s.abs(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(s.abs(e.File), dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			e.File, e.Size = "", 0
			return nil
		}
		return err
	}
	removeEmptyParents(filepath.Dir(s.abs(e.File)), filepath.Join(s.root, pinnedDir))
	e.File = rel
	return nil
}

// removeEmptyParents removes dir and its parents up to, not including, stop
// while they are empty, so demoting an artist's last track removes the
// artist directory.
func removeEmptyParents(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// AddSource makes src hold key. A cached file moves to pinned/. It reports
// whether the track still needs downloading.
func (s *Store) AddSource(m Meta, src string) (needsDownload bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(m)
	if !slices.Contains(e.Sources, src) {
		e.Sources = append(e.Sources, src)
		sort.Strings(e.Sources)
	}
	if e.File != "" {
		if err := s.relocateLocked(e, pinnedRel(e, filepath.Ext(e.File))); err != nil {
			return false, err
		}
	}
	return e.File == "", s.saveLocked()
}

// pinAll makes src hold every track in metas, in one index write.
func (s *Store) pinAll(metas []Meta, src string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range metas {
		e := s.entryLocked(m)
		if !slices.Contains(e.Sources, src) {
			e.Sources = append(e.Sources, src)
			sort.Strings(e.Sources)
		}
		if e.File != "" {
			if err := s.relocateLocked(e, pinnedRel(e, filepath.Ext(e.File))); err != nil {
				return err
			}
		}
	}
	return s.saveLocked()
}

// RemoveSource lets src go of key. When no source holds the track any more,
// its file is demoted to cache/, keeping its last-played time, and the
// cache is evicted as needed.
func (s *Store) RemoveSource(key, src string) error {
	return s.removeSources(map[string][]string{key: {src}})
}

// removeSources removes several sources in one index write.
func (s *Store) removeSources(drop map[string][]string) error {
	if len(drop) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, srcs := range drop {
		e, ok := s.tracks[key]
		if !ok {
			continue
		}
		e.Sources = slices.DeleteFunc(e.Sources, func(v string) bool { return slices.Contains(srcs, v) })
		if len(e.Sources) == 0 {
			e.Sources = nil
			if err := s.relocateLocked(e, cacheRel(e.Key, filepath.Ext(e.File))); err != nil {
				return err
			}
		}
	}
	if _, err := s.evictLocked(); err != nil {
		return err
	}
	return s.saveLocked()
}

// HeldBy returns the keys that src holds.
func (s *Store) HeldBy(src string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for k, e := range s.tracks {
		if slices.Contains(e.Sources, src) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Sources returns every pin source that holds at least one track.
func (s *Store) Sources() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for _, e := range s.tracks {
		for _, src := range e.Sources {
			seen[src] = true
		}
	}
	out := make([]string, 0, len(seen))
	for src := range seen {
		out = append(out, src)
	}
	sort.Strings(out)
	return out
}

// PinnedMissing returns the pinned keys that have no file yet.
func (s *Store) PinnedMissing() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	for _, e := range s.tracks {
		if len(e.Sources) > 0 && e.File == "" {
			out = append(out, copyEntry(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// SetList stores the keys of a listing so the list can be shown offline.
func (s *Store) SetList(id string, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Equal(s.lists[id], keys) {
		return nil
	}
	s.lists[id] = slices.Clone(keys)
	return s.saveLocked()
}

// List returns the stored keys of a listing.
func (s *Store) List(id string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys, ok := s.lists[id]
	return slices.Clone(keys), ok
}

// OnDisk returns every track with a complete file, most recently played
// first.
func (s *Store) OnDisk() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	for _, e := range s.tracks {
		if e.File != "" {
			out = append(out, copyEntry(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastPlayed != out[j].LastPlayed {
			return out[i].LastPlayed > out[j].LastPlayed
		}
		if out[i].Artist != out[j].Artist {
			return out[i].Artist < out[j].Artist
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// HasAny reports whether any track has audio on disk.
func (s *Store) HasAny() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.tracks {
		if e.File != "" || e.PartialSize > 0 {
			return true
		}
	}
	return false
}

// Stats returns the sizes of the cache and the pinned store.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{CacheMax: s.cacheMax}
	for _, e := range s.tracks {
		switch e.State() {
		case StatePinned:
			st.PinnedBytes += e.Size
			st.PinnedTracks++
		case StateCached:
			st.CacheBytes += e.Size + e.PartialSize
			st.CacheTracks++
		case StatePartial:
			st.CacheBytes += e.PartialSize
		}
	}
	return st
}

// Evict deletes cached files and partial downloads, least recently played
// first, until the cache fits cacheMax. It returns the evicted keys.
func (s *Store) Evict() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted, err := s.evictLocked()
	if err != nil || len(evicted) == 0 {
		return evicted, err
	}
	return evicted, s.saveLocked()
}

// evictLocked is Evict without the lock and the index write. Pinned files,
// and tracks that are playing or downloading, are never evicted.
func (s *Store) evictLocked() ([]string, error) {
	var used int64
	var candidates []*Entry
	for _, e := range s.tracks {
		if len(e.Sources) > 0 {
			// A pinned track's leftover partial would be odd, but count it.
			used += e.PartialSize
			continue
		}
		size := e.PartialSize
		if e.File != "" {
			size += e.Size
		}
		if size == 0 {
			continue
		}
		used += size
		if s.busy[e.Key] == 0 {
			candidates = append(candidates, e)
		}
	}
	if used <= s.cacheMax {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].LastPlayed != candidates[j].LastPlayed {
			return candidates[i].LastPlayed < candidates[j].LastPlayed
		}
		return candidates[i].Key < candidates[j].Key
	})
	var evicted []string
	for _, e := range candidates {
		if used <= s.cacheMax {
			break
		}
		if e.File != "" {
			if err := os.Remove(s.abs(e.File)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return evicted, err
			}
			used -= e.Size
			e.File, e.Size = "", 0
		}
		if e.PartialSize > 0 {
			removePartialFiles(s.partialPrefix(e.Key), false)
			used -= e.PartialSize
			e.PartialSize = 0
		}
		evicted = append(evicted, e.Key)
	}
	return evicted, nil
}
