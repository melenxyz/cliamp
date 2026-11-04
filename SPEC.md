# cliamp custom SoundCloud provider: spec

Status: milestones 1–6 implemented in `external/soundcloudx/` (2026-11-04) · Started 2026-11-04 · Based on cliamp `02309c9` (2026-10-22, v2.3.0)

## Goal

A SoundCloud provider for cliamp that streams like the current one but keeps
what it plays, so music becomes available offline over time.

Every track lives in one of three states:

| State | How it got there | Evicted automatically? |
|---|---|---|
| **Remote** | Never played or downloaded | n/a |
| **Cached** | Played once (streamed and saved at the same time) | Yes, least recently played first, when the cache is over its size limit |
| **Pinned** | Part of a sync rule (my Likes, all of artist X) or pinned by hand | Never, and doesn't count toward the cache limit |

## Features

### 1. Stream and save at the same time
- The first play of a track starts playback right away and writes the file to
  disk while it plays.
- If playback finishes (or the download finishes in the background), the file
  is committed to the cache.
- **If I skip early, keep the partial download and resume it later.** The next
  play (or a sync job) continues from where it stopped instead of starting over.
  Partials are only thrown away if resuming fails (see below).
- Later plays come from the local file: instant start, seekable, works offline.

#### Resume: tested 2026-11-04 (yt-dlp 2026.10.26)
- SoundCloud now serves **HLS only** (`hls_aac_96k`, `hls_aac_160k` m4a; the
  original upload format needs a signed-in account). No progressive MP3.
- yt-dlp downloads HLS in fragments (22 for a 3:34 track) and leaves three files
  when interrupted:
  - `<name>.part`: the fragments finished so far, concatenated
  - `<name>.ytdl`: resume state, e.g. `{"downloader": {"current_fragment": {"index": 3}}}`
  - `<name>.part-FragN.part`: the fragment that was in progress
- Running the same command again **resumes at that fragment** (log showed
  `frag 3/22` at 18%) and produced a valid file (ffprobe: 213.9 s).
- **Catch:** a leftover `-FragN.part` caused `HTTP Error 416: Requested Range
  Not Satisfiable` retries in one run. Rule: **delete `*-Frag*.part` before
  resuming** and keep `.part` + `.ytdl`. That loses at most one fragment (~10 s
  of audio).
- **Discard rule:** if a resume fails (yt-dlp error, or the stream's fragment
  list changed), delete `.part`, `.ytdl` and fragments, and start fresh next time.
- Partials count toward `cache_max` and are evicted like cached tracks.
- An interrupted HLS download stops mid-way through its fragments, so the
  stream URLs can expire. yt-dlp re-extracts on resume, which worked in the test.

### 2. Pinned downloads (sync rules)
- Rules are set in config, e.g.:
  ```toml
  [soundcloud_custom]
  user = "yourname"
  cookies_from = "firefox"
  cache_max = "20GB"

  [[soundcloud_custom.sync]]
  source = "likes"            # my likes

  [[soundcloud_custom.sync]]
  source = "artist:forss"     # everything forss uploaded
  ```
- A sync job fetches each rule's track list and downloads anything missing.
  Already-downloaded tracks are skipped (yt-dlp `--download-archive` style).
- When a cached track becomes pinned, it moves to the pinned store; nothing is
  downloaded again.
- Pin or unpin a single track by hand from the UI (key binding TBD).
- **Pin sources are generic.** A track is pinned while _at least one_ source
  holds it. Sources so far: my likes, an artist's uploads, a manual pin. More
  later (a playlist/set, reposts, ...). Each track records which sources hold it.
- **When the last source lets go** (I unlike it, remove the artist rule, unpin
  it by hand, the artist deletes the track), the file is **demoted to the
  cache**, not deleted. It then follows normal cache eviction, and its
  last-played time stays as it was.
- Demoting moves the file from `pinned/` to `cache/` (a rename on the same
  filesystem, so no copy).

### 3. Cache with a size limit
- `cache_max` limits the cache only. Pinned files are never counted or evicted.
- Eviction is least recently **played** first, and runs after each new cache
  write.
- Cache stats visible somewhere (size used / limit, number of tracks).

### 4. Browse
- **Likes** for my user (already in the upstream provider).
- **Artists**: a list of artists from config, each opening that artist's tracks.
- **Offline**: everything on disk (cached + pinned), playable with no network.
- A per-track marker in the list: pinned / cached / remote.
- Search (`Ctrl+F`) works as in upstream.

### 5. Offline behaviour
- No network: lists come from the local index, and only on-disk tracks play.
- Remote tracks in an offline list are shown greyed out or skipped.

## How it fits into cliamp

What I found reading the source:

- Providers are **compiled Go packages** in `external/<name>/`. Lua plugins can
  only be `hook` or `visualizer` plugins, so this has to be a **fork of
  cliamp** (or an upstream PR), not a plugin.
- The upstream provider is tiny (`external/soundcloud/provider.go`, ~100 lines).
  It lists `soundcloud.com/<user>/{tracks,likes,reposts}` and hands everything
  to yt-dlp through `resolve.ResolveYTDLBatch`.
- Playback of yt-dlp tracks today: `player/ytdl.go` runs `yt-dlp -o -` piped
  into `ffmpeg`, which outputs PCM. Nothing is written to disk and seeking is
  limited.
- **The hook we need:** `provider.CustomStreamer` (`provider/interfaces.go`).
  A provider declares its own URI scheme (e.g. `sccache:<track-id>`) and returns
  its own `beep.StreamSeekCloser`. Spotify uses this
  (`external/spotify/streamer.go`). The whole stream-and-save logic can live
  inside the provider without patching the player.
- `tracksave/` already downloads a yt-dlp track to `~/Music/cliamp` (the
  "save track" feature). That's the starting point for the downloader.
- `provider.PlaybackStateReporter` can draw a marker column per row with no
  I/O. That could be how the pinned/cached/remote marker gets shown.
- The player's decode helpers are not exported, so the provider either runs
  its own ffmpeg-to-PCM pipeline (like Spotify does) or we export a small
  helper from `player/`.

### Stream-while-downloading: candidate designs

**A. Tee in the provider (preferred)**
`yt-dlp -o <cache>/<id>.part` writes the file. Our streamer reads the growing
`.part` file, waiting at EOF while yt-dlp is still running, and feeds ffmpeg.
When yt-dlp exits successfully, rename `.part` to the final name. One network
fetch, and seeking is possible inside the part already downloaded.

**B. Tee the pipe**
`yt-dlp -o -` → `io.TeeReader` → (ffmpeg, file). Simpler, but only works for
formats yt-dlp can write to stdout, and the saved file may need remuxing.

**C. Download first, then play**
Start the download and play the local file once enough is buffered. Easiest,
but adds start-up delay.

Format check done: SoundCloud is HLS-only (fragmented m4a), so yt-dlp's
`.part` file grows one fragment at a time. To verify for design A: ffmpeg can
decode a growing fragmented-m4a `.part` file sequentially, and the final
renamed `.m4a` is seekable (needs testing; yt-dlp may run a fixup step at the
end).

## Storage layout (draft)

```
~/Music/soundcloud/            # or configurable
  pinned/<artist>/<title> [<id>].<ext>
  cache/<id>.<ext>
  partial/<id>.m4a.part        # + .ytdl resume state; counts as cache
  index.db                     # or index.toml/json: id, title, artist, duration,
                               # state, size, last_played, pin sources
```

Pinned files get readable names so they're usable outside cliamp. Cache files
are keyed by ID because they're cliamp-internal.

## Repo / workflow plan

- [x] Fork `bjarneo/cliamp` and clone it into this folder.
- [x] Decide: new provider (`external/soundcloudx/`) beside the upstream one,
      or extend `external/soundcloud/` (better chance of an upstream PR).
      → new provider (private fork, fewer rebase conflicts).
- [x] Build locally with `make`, and run the fork's binary next to the pacman
      cliamp (don't replace `/usr/bin/cliamp`).
- [ ] Keep the fork rebased on upstream.

## Milestones

All six are implemented. Config reference: `docs/configuration.md`, section
"SoundCloud+ (offline cache)".

1. **Artists list**: config `artists = [...]`, an "Artists" section. Small,
   gets familiar with the codebase.
2. **Local index + offline list**: index file, an "Offline" playlist of files on disk.
3. **Stream-and-save**: `CustomStreamer` with design A; cache commit on finish.
4. **Cache eviction**: `cache_max`, LRU by last played.
5. **Sync rules**: pinned downloads for likes and artists, run from a CLI
   command (`cliamp plugins call`?) and/or a systemd timer.
6. **UI polish**: state markers, pin/unpin key, cache stats.

## Open questions

- Where does sync run: inside cliamp (background job), as a separate CLI or
  timer, or both? --mélen added: inside cliamp → done: 15 s after start, every
  `sync_interval`, and on `Ctrl+R`.
- Audio format: keep whatever SoundCloud serves, or always remux/convert to one
  format? → kept as served (`hls_aac_160k` m4a, remuxed by yt-dlp's FixupM4a).
- Reposts and playlists/sets: in scope for sync rules?
- Is the plan to send this upstream as a PR, or keep it as a private fork? --mélen added: private fork

## Decisions

- **2026-11-04: Skipped tracks** keep their partial download and resume later.
  Partials are discarded only when a resume fails. Resume was tested and works
  (see feature 1).
- **2026-11-04: Leaving pinning:** when no pin source holds a track any more,
  its file is demoted to the cache, not deleted.
- **2026-11-04: Package and config name:** `external/soundcloudx/`, provider
  key `soundcloudx` (alias `scx`), config section `[soundcloudx]`, display
  name "SoundCloud+". Not `soundcloud_custom`: the config golden test and the
  docs tests only accept `[a-z]+` section and provider names.
- **2026-11-04: Sync rules are a list,** `sync = ["likes", "artist:forss"]`,
  not `[[soundcloudx.sync]]` tables: cliamp's hand-written config parser has
  no arrays of tables. `artists = [...]` lists the Artists section.
- **2026-11-04: Manual pin = ♥ favorite.** The provider implements
  `provider.TrackFavoriter`, so favoriting a track pins it and removing the
  favorite unpins it. No new key binding.
- **2026-11-04: Stream-and-save is design A,** verified: ffmpeg decodes the
  growing fragmented-MP4 `.part` file fed through stdin by a tail reader, and
  the decoder keeps its file handle across yt-dlp's rename and fixup. The
  provider runs its own ffmpeg (no `player/` changes).
- **2026-11-04: Seeking during a download** is allowed inside the part already
  downloaded (estimated at 160 kbps); after the download is committed, seeks
  use the final file.
- **2026-11-04: Markers:** `●` pinned, `◐` cached, `◷` partial (changed the
  same day from `○` cached / `◐` partial: half a circle reads better as
  "cached", and partials are rare enough for a clock). Upstream's
  `PlaybackState` gained an optional `Marker` field (small patch in
  `provider/interfaces.go`, `ui/model/view.go`, `ui/model/inline_overlays.go`).
- **2026-11-04: Index** is `index.json` in the storage root (no new
  dependency), written atomically on every change. It also keeps the last
  listing of each list, for offline browsing.
- **2026-11-04: Nerd Font icons.** The Unicode heart looked bigger than the
  circles, so the heart (filled and outline) and the three state markers now
  use Font Awesome glyphs from the Nerd Font (U+F004/U+F08A heart,
  U+F111 pinned, U+F042 cached, U+F017 partial). They need a Nerd Font, which
  every terminal on this machine uses. `favHeart` in `ui/model/view.go` is the
  one place to change back to `"♥\uFE0E"`.
- **2026-11-04: Naming:** rows read `Artist – Title` (en dash, so it stands
  apart from hyphens inside titles). The artist is the uploader's display
  name, learned from search results and downloads and reused for the slug-only
  artist pages. A title that starts with the uploader's name drops it. Shown
  through a new `provider.MetaDisplayName` override in `trackViewName`.
- **2026-11-04: Hearts:** a filled ♥ always shows on favorites; an empty ♡
  shows only on the highlighted row (playlist and search results). Search rows
  also show the state marker and the duration.
- **2026-11-04: Favorite artist = `c`** ("creator"; lowercase `h` is EQ left). It adds the uploader to the Artists
  section (saved to `artists` in the config) or removes it. It downloads
  nothing; an `artist:<name>` sync rule is how a catalogue gets pinned. New
  capability `provider.ArtistFavoriter`.
- **2026-11-04: Footer:** pinned downloads (favorites, sync) show
  `↓ Pinning <track>… 40%`, then `● Pinned <track>` or the error. Downloads
  that only playback wants stay quiet.
- **2026-11-04: Durations** for artist pages and likes come from a background
  yt-dlp extraction (10 tracks per call, 3 calls at a time, 150 tracks per list
  open), saved to the index so each track is looked up once. `Ctrl+R` reloads
  the open list in place (`RefreshablePlaylist`) to show them. Calling
  SoundCloud's api-v2 directly with yt-dlp's cached client_id would give
  durations in one request, but it was ruled out: it means using a scraped
  private-API key, which Claude Code's auto mode blocked as access
  circumvention. Revisit only as an explicit decision.
- **2026-11-04: Gapless preload** of the next track starts its download too,
  so the next track is usually on disk before it plays.

## Notes

- Requires `yt-dlp` (installed via mise, kept current by `omarchy update`) and
  `ffmpeg`.
- Downloads are for personal offline listening only.
