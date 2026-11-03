package soundcloudx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bjarneo/cliamp/applog"
)

// syncStartDelay keeps the first sync off the start-up path.
var syncStartDelay = 15 * time.Second

// rule is one configured sync rule.
type rule struct {
	source string // the pin source it records, e.g. "likes" or "artist:forss"
	page   string // the SoundCloud page that lists its tracks
	list   string // the playlist ID whose saved listing it refreshes
}

// rules parses the sync rules. An invalid rule is logged and skipped.
func (p *Provider) rules() []rule {
	var out []rule
	for _, raw := range p.cfg.Sync {
		src := strings.TrimSpace(raw)
		switch {
		case src == "likes":
			if p.cfg.User == "" {
				applog.UserWarn("soundcloudx: sync rule %q needs user to be set", src)
				continue
			}
			out = append(out, rule{source: src, page: pageBase + url.PathEscape(p.cfg.User) + "/likes", list: listLikes})
		case strings.HasPrefix(src, artistList) && len(src) > len(artistList):
			a := strings.TrimPrefix(src, artistList)
			out = append(out, rule{source: src, page: pageBase + url.PathEscape(a) + "/tracks", list: artistList + a})
		default:
			applog.UserWarn("soundcloudx: unknown sync rule %q (want likes or artist:<name>)", src)
		}
	}
	return out
}

// startSyncLoop runs the sync rules shortly after start, on every interval
// and on Refresh.
func (p *Provider) startSyncLoop() {
	p.loopDone = make(chan struct{})
	interval := p.cfg.SyncInterval
	if interval == 0 {
		interval = DefaultSyncInterval
	}
	go func() {
		defer close(p.loopDone)
		timer := time.NewTimer(syncStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-p.life.Done():
				return
			case <-timer.C:
			case <-p.syncNow:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			if err := p.Sync(p.life); err != nil && !errors.Is(err, context.Canceled) {
				applog.UserWarn("soundcloudx: sync: %v", err)
			}
			timer.Reset(interval)
		}
	}()
}

// Sync applies the sync rules: it lists each rule's tracks, pins what the
// rule holds, lets go of what it no longer holds, and downloads the pinned
// tracks that are missing. A rule whose listing fails changes nothing, so a
// network error never demotes a pinned track.
func (p *Provider) Sync(ctx context.Context) error {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()

	rules := p.rules()
	configured := map[string]bool{SourceManual: true}
	for _, r := range rules {
		configured[r.source] = true
	}

	// Rules removed from the config let go of their tracks.
	drop := map[string][]string{}
	for _, src := range p.store.Sources() {
		if configured[src] {
			continue
		}
		for _, k := range p.store.HeldBy(src) {
			drop[k] = append(drop[k], src)
		}
	}

	var failed []string
	for _, r := range rules {
		if err := ctx.Err(); err != nil {
			return err
		}
		metas, err := p.list(ctx, r.page)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.source, err))
			continue
		}
		keys := make([]string, len(metas))
		holds := make(map[string]bool, len(metas))
		for i, m := range metas {
			keys[i] = m.Key
			holds[m.Key] = true
		}
		if err := p.store.pinAll(metas, r.source); err != nil {
			return err
		}
		if err := p.store.SetList(r.list, keys); err != nil {
			return err
		}
		for _, k := range p.store.HeldBy(r.source) {
			if !holds[k] {
				drop[k] = append(drop[k], r.source)
			}
		}
	}
	if err := p.store.removeSources(drop); err != nil {
		return err
	}

	missing := p.store.PinnedMissing()
	if len(missing) > 0 {
		applog.Status("soundcloudx: downloading %d pinned tracks", len(missing))
	}
	done, errs := 0, 0
	for _, e := range missing {
		j := p.dl.pin(Meta{Key: e.Key, Permalink: e.Permalink, Title: e.Title, Artist: e.Artist, Duration: e.Duration})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-j.done:
		}
		if j.err != nil {
			errs++
			applog.Warn("soundcloudx: download %s: %v", e.Key, j.err)
			continue
		}
		done++
	}
	if len(missing) > 0 {
		applog.Status("soundcloudx: sync done, %d downloaded, %d failed", done, errs)
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}
