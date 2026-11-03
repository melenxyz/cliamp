# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This is a **private fork** of `bjarneo/cliamp`. The root `CLAUDE.md` (and `AGENTS.md`, a symlink to it) is upstream's file: it holds the architecture map, the build targets and the conventions, and all of that applies here. This file only adds what is specific to the fork. Leave the root `CLAUDE.md` alone so rebases on upstream stay conflict-free, and put fork notes here.

## What the fork is for

The fork's provider lives in `external/soundcloudx/` (config `[soundcloudx]`, display name "SoundCloud+"). How the pieces fit:

- `store.go`: `index.json` plus the state machine (remote → partial → cached → pinned), pin sources, demotion, LRU eviction. Every other piece goes through it.
- `download.go`: one yt-dlp job per track, shared by playback and sync. A job that only streamers want is cancelled when the last streamer closes, which leaves the partial download in place to resume later.
- `streamer.go` + `tail.go`: the `CustomStreamer`. It runs its own ffmpeg on either the committed file or the growing `.part` file (fed through a tail reader), and seeks by restarting ffmpeg.
- `sync.go`: the sync rules. A listing that fails changes nothing, so a network error never demotes a track.
- Upstream patches are kept small: registration in `providers.go`, the config section in `config/config.go`, and `PlaybackState.Marker` in `provider/interfaces.go` plus the two marker renderers in `ui/model/`. `SPEC.md` → Decisions explains each one.

`SPEC.md` is the source of truth: a SoundCloud provider that streams like upstream's `external/soundcloud/` but saves what it plays, so tracks move between three states: **remote → cached (LRU-evicted, `cache_max`) → pinned (sync rules, never evicted)**. Read `SPEC.md` before starting any work on the provider, and record new decisions in its **Decisions** section with a date.

Key points from the spec that take reading several files to work out:

- Providers are compiled Go packages in `external/<name>/`. A Lua plugin cannot be a provider, which is why this is a fork.
- The integration point is `provider.CustomStreamer` (`provider/interfaces.go`). The provider owns a URI scheme and returns its own `beep.StreamSeekCloser`, so stream-and-save can live inside the provider without patching `player/`. `external/spotify/streamer.go` is the working example, and it runs its own ffmpeg-to-PCM pipeline because the decode helpers in `player/` are not exported.
- Today, yt-dlp playback is `player/ytdl.go` (`yt-dlp -o -` | ffmpeg → PCM) and writes nothing to disk. `tracksave/` already downloads a yt-dlp track to disk, so start the downloader from it.
- `provider.PlaybackStateReporter` is the likely way to draw the pinned/cached/remote marker.
- SoundCloud serves HLS only (fragmented m4a). Resuming partial downloads relies on yt-dlp's `.part` + `.ytdl` files. Delete `*-Frag*.part` before resuming, because a leftover fragment causes HTTP 416 errors.
- Sync runs inside cliamp, as a background job.

## Running the fork next to the system cliamp

Pacman installs `/usr/bin/cliamp`. Don't replace it, and don't use `make install` (`~/.local/bin` comes after `/usr/bin` in PATH here anyway). Build with `mise exec -- make build` and run `./cliamp` from the repo.

Both binaries read `~/.config/cliamp/` and use the same IPC socket there, so they can't run at the same time with the default config. Give the fork its own config directory with `CLIAMP_CONFIG_DIR=/tmp/... ./cliamp`, or follow upstream's isolated-HOME advice for test runs.

Go isn't on the global PATH on this machine. It comes from the `mise.toml` pin, so prefix every `go`/`make` command with `mise exec --`.

Run a single test: `mise exec -- go test ./external/soundcloudx/ -run TestName -v`

The provider's own tests use a fake yt-dlp script and the real ffmpeg. `SOUNDCLOUDX_LIVE=1 mise exec -- go test -run Live -v ./external/soundcloudx/` runs the whole flow against soundcloud.com (needs network).

To try the binary headless without touching `~/.config/cliamp`, run it with `HOME` and `XDG_*` set to a short temp directory (the IPC socket path must be under 108 bytes, so the scratchpad is too deep), then drive it with `./cliamp remote call --wait --params '{"provider":"soundcloudx",...}' provider.load`. Stop it by PID: `pkill -f` matches its own shell.

## Tests that a new provider or config key can trip

- `config/config_golden_test.go` loads every setting in `config.toml.example`, and `docs_providers_test.go` / `docs_links_test.go` check provider lists and anchors in `docs/` and `README.md`. A new `[soundcloudx]`-style section or a provider registered in `providers.go` will likely need matching entries there for `make test` to pass.
- Upstream's rule to keep `docs/` and `site/index.html` in sync matters less for a private fork, but the tests still read both files.

## Git remotes

- `upstream`: `https://github.com/bjarneo/cliamp.git`. Rebase `main` onto `upstream/main` to stay current; don't merge it.
- `origin`: the user's GitHub fork (private fork, no upstream PRs planned).
