// Package engine turns a playlist definition into the desired episode queue and
// reconciles the Audiobookshelf playlist with it in place (no delete/recreate).
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/projector22/abp/internal/abp"
	"github.com/projector22/abp/internal/config"
)

type Item struct {
	LibraryItemID string
	EpisodeID     string
	Podcast       string
	Title         string
	Published     time.Time
	Seconds       float64
}

type Queue struct {
	Items      []Item // oldest first, truncated to the playlist limit
	Unfinished int    // total unfinished episodes before truncation
	Warnings   []string
}

// BuildQueue collects every unfinished episode of the playlist's podcasts,
// sorts oldest -> newest by publish date and keeps the first `Limit`.
func BuildQueue(ctx context.Context, c *abp.Client, pl config.Playlist) (*Queue, error) {
	// 1. Which episodes has the user already finished? Progress is keyed by
	// library item (the podcast) plus episode id.
	me, err := c.Me(ctx)
	if err != nil {
		return nil, err
	}
	finished := finishedEpisodes(me)

	type result struct {
		det *abp.PodcastDetail
		err error
	}
	// 2. Fetch every podcast's episode list concurrently (at most 4 requests in
	// flight so we don't hammer the server). Each goroutine writes only to its
	// own slot of results, so no locking is needed.
	results := make([]result, len(pl.Podcasts))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, p := range pl.Podcasts {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := c.PodcastDetail(ctx, id)
			results[i] = result{d, err}
		}(i, p.ID)
	}
	wg.Wait()

	// 3. Merge: collect every unfinished episode from every podcast.
	q := &Queue{}
	var all []Item
	for i, r := range results {
		p := pl.Podcasts[i]
		if r.err != nil {
			if errors.Is(r.err, abp.ErrNotFound) {
				q.Warnings = append(q.Warnings, fmt.Sprintf("%q no longer exists in Audiobookshelf (skipped)", p.Title))
				continue
			}
			// Any other failure: abort rather than shrink the playlist on a blip.
			return nil, fmt.Errorf("%s: %w", p.Title, r.err)
		}
		title := r.det.Title
		if title == "" {
			title = p.Title
		}
		for _, e := range r.det.Episodes {
			if finished[p.ID+"/"+e.ID] {
				continue
			}
			all = append(all, Item{
				LibraryItemID: p.ID, EpisodeID: e.ID, Podcast: title,
				Title: e.Title, Published: e.Published(), Seconds: e.Seconds(),
			})
		}
	}
	// 4. Oldest first. The tie-breakers (podcast, title, id) make the order fully
	// deterministic, so two syncs with the same data never reshuffle the list.
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if !a.Published.Equal(b.Published) {
			return a.Published.Before(b.Published)
		}
		if a.Podcast != b.Podcast {
			return a.Podcast < b.Podcast
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.EpisodeID < b.EpisodeID
	})
	// 5. Keep only the oldest `limit` episodes; Unfinished remembers the full count.
	q.Unfinished = len(all)
	limit := pl.Limit
	if limit <= 0 {
		limit = config.DefaultLimit
	}
	if len(all) > limit {
		all = all[:limit]
	}
	q.Items = all
	return q, nil
}

// finishedEpisodes returns the set of episodes the user has finished, keyed by
// library item id plus episode id.
func finishedEpisodes(me *abp.Me) map[string]bool {
	finished := map[string]bool{}
	for _, p := range me.MediaProgress {
		if p.IsFinished && p.EpisodeID != "" {
			finished[p.LibraryItemID+"/"+p.EpisodeID] = true
		}
	}
	return finished
}

// Remaining is how many unfinished episodes a playlist's podcasts hold in total,
// i.e. how many are still to go before the playlist's podcasts are all caught up.
// It is the same number BuildQueue reports as Queue.Unfinished. Err is set (and
// Unfinished meaningless) when one of the playlist's podcasts could not be read.
type Remaining struct {
	Unfinished int
	Err        error
}

// CountUnfinished counts, for each playlist, the unfinished episodes across its
// podcasts. podcasts maps a playlist id to the ids of its podcasts. It reads the
// user's progress once and each distinct podcast once, however many playlists
// share it, and writes nothing. A podcast that no longer exists is skipped, as in
// BuildQueue. Any other failure marks only the playlists using that podcast; an
// error is returned only when the progress itself cannot be read.
func CountUnfinished(ctx context.Context, c *abp.Client, podcasts map[string][]string) (map[string]Remaining, error) {
	me, err := c.Me(ctx)
	if err != nil {
		return nil, err
	}
	finished := finishedEpisodes(me)

	// Fetch each distinct podcast once, at most 4 requests in flight.
	type result struct {
		unfinished int
		missing    bool
		err        error
	}
	var ids []string
	seen := map[string]bool{}
	for _, list := range podcasts {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	results := make([]result, len(ids))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := c.PodcastDetail(ctx, id)
			if err != nil {
				results[i] = result{missing: errors.Is(err, abp.ErrNotFound), err: err}
				return
			}
			n := 0
			for _, e := range d.Episodes {
				if !finished[id+"/"+e.ID] {
					n++
				}
			}
			results[i] = result{unfinished: n}
		}(i, id)
	}
	wg.Wait()
	byID := make(map[string]result, len(ids))
	for i, id := range ids {
		byID[id] = results[i]
	}

	out := make(map[string]Remaining, len(podcasts))
	for plID, list := range podcasts {
		var r Remaining
		for _, id := range list {
			pr := byID[id]
			switch {
			case pr.missing:
				continue
			case pr.err != nil:
				r.Err = pr.err
			default:
				r.Unfinished += pr.unfinished
			}
		}
		out[plID] = r
	}
	return out, nil
}

type Result struct {
	Action     string // created | updated | unchanged | emptied | nothing to do
	Count      int
	Unfinished int
	Added      int
	Removed    int
	Warnings   []string
	Notes      []string // non-fatal observations (e.g. server order differs)
	Items      []Item
}

// Summary renders a one-line description of what a sync did, such as
// "updated · 25 items (+3 −2)". It is stored as the playlist's last result and
// shown in the UI and the cron log.
func (r *Result) Summary() string {
	s := fmt.Sprintf("%s · %d items", r.Action, r.Count)
	if r.Added > 0 || r.Removed > 0 {
		s += fmt.Sprintf(" (+%d −%d)", r.Added, r.Removed)
	}
	if len(r.Notes) > 0 {
		s += " · " + r.Notes[0]
	}
	return s
}

// key identifies a playlist entry for comparisons: library item id plus episode
// id. Both must match because one podcast contributes many episodes.
func key(it abp.PlaylistItem) string { return it.LibraryItemID + "/" + it.EpisodeID }

// Run reconciles one playlist with the server and records the outcome in st.
//
// The flow is: build the desired queue, locate the playlist on the server, then
// (a) create it if it does not exist, (b) leave it alone if it already matches,
// (c) delete its contents if the queue is empty, or (d) update it in place. With
// dry=true nothing is written to the server or to the state file.
//
// Updating in place is subtle because Audiobookshelf's add and remove endpoints
// do not keep the item numbering tidy; see the comment on the update branch.
func Run(ctx context.Context, c *abp.Client, pl config.Playlist, st *config.State, dry bool) (res *Result, err error) {
	defer func() {
		if dry {
			return
		}
		st.Update(pl.ID, func(p *config.PlaylistState) {
			p.LastSync = time.Now()
			if err != nil {
				p.Error = err.Error()
			} else {
				p.Error = ""
				p.Summary = res.Summary()
			}
		})
	}()

	// Work out what the playlist should contain, as plain (item, episode) pairs.
	q, err := BuildQueue(ctx, c, pl)
	if err != nil {
		return nil, err
	}
	desired := make([]abp.PlaylistItem, len(q.Items))
	for i, it := range q.Items {
		desired[i] = abp.PlaylistItem{LibraryItemID: it.LibraryItemID, EpisodeID: it.EpisodeID}
	}
	res = &Result{Count: len(desired), Unfinished: q.Unfinished, Warnings: q.Warnings, Items: q.Items}

	// Look for the playlist on the server (by remembered id, else by name).
	existing, err := find(ctx, c, pl, st)
	if err != nil {
		return nil, err
	}

	// Case (a): no playlist on the server yet, so create it with the right order.
	if existing == nil {
		res.Added = len(desired)
		if len(desired) == 0 {
			res.Action = "nothing to do"
			return res, nil
		}
		res.Action = "created"
		if dry {
			return res, nil
		}
		p, err := c.CreatePlaylist(ctx, pl.Library, pl.Name, pl.Description, desired)
		if err != nil {
			return nil, err
		}
		st.Update(pl.ID, func(s *config.PlaylistState) { s.ABSID = p.ID })
		return res, nil
	}

	// Remember the server's id so a later rename in config.yaml can't orphan it.
	st.Update(pl.ID, func(s *config.PlaylistState) { s.ABSID = existing.ID })

	// Compare as sets to count what was added/removed for the summary.
	have := map[string]bool{}
	for _, it := range existing.Items {
		have[key(it)] = true
	}
	want := map[string]bool{}
	for _, it := range desired {
		want[key(it)] = true
		if !have[key(it)] {
			res.Added++
		}
	}
	for _, it := range existing.Items {
		if !want[key(it)] {
			res.Removed++
		}
	}
	// The description can't be cleared through the API (the server ignores an
	// empty one), so only treat a non-empty difference as a change.
	metaChanged := existing.Name != pl.Name || (pl.Description != "" && existing.Description != pl.Description)
	var missing, unwanted []abp.PlaylistItem
	for _, it := range desired {
		if !have[key(it)] {
			missing = append(missing, it)
		}
	}
	for _, it := range existing.Items {
		if !want[key(it)] {
			unwanted = append(unwanted, it)
		}
	}
	itemsChanged := len(missing) > 0 || len(unwanted) > 0 || !sameOrder(existing.Items, desired)

	switch {
	case !itemsChanged && !metaChanged:
		res.Action = "unchanged"
	case len(desired) == 0:
		// Audiobookshelf deletes a playlist once its last item is removed.
		res.Action = "emptied"
		if !dry && len(existing.Items) > 0 {
			if err := c.RemoveItems(ctx, existing.ID, existing.Items); err != nil {
				return nil, err
			}
			st.Update(pl.ID, func(s *config.PlaylistState) { s.ABSID = "" })
		}
	default:
		res.Action = "updated"
		if dry {
			return res, nil
		}
		// Audiobookshelf's add/remove endpoints don't keep item order tidy:
		// removing leaves gaps in the numbering and adding numbers new items
		// "count+1", which lands them in the middle after a removal. So:
		//   1. add what's missing   (first, so the playlist is never emptied,
		//                            which would make the server delete it)
		//   2. remove what's unwanted
		//   3. PATCH the full list once, which renumbers everything 1..n.
		//      The server only accepts this when the set of items already
		//      matches, which steps 1-2 guarantee.
		if len(missing) > 0 {
			if err := c.AddItems(ctx, existing.ID, missing); err != nil {
				return nil, fmt.Errorf("add items: %w", err)
			}
		}
		if len(unwanted) > 0 {
			if err := c.RemoveItems(ctx, existing.ID, unwanted); err != nil {
				return nil, fmt.Errorf("remove items: %w", err)
			}
		}
		var order []abp.PlaylistItem
		if itemsChanged {
			order = desired
		}
		if err := c.UpdatePlaylist(ctx, existing.ID, pl.Name, pl.Description, order); err != nil {
			return nil, fmt.Errorf("reorder/update: %w", err)
		}
		if itemsChanged {
			// Never claim success unless the server really has the right list.
			got, err := c.GetPlaylist(ctx, existing.ID)
			if err != nil {
				return nil, fmt.Errorf("verify: %w", err)
			}
			if !sameOrder(got.Items, desired) {
				kind, detail := diagnose(desired, got.Items)
				file := writeDiag(pl, q.Items, got.Items, detail)
				if kind == "members" {
					return nil, fmt.Errorf("playlist on the server does not match after update: %s (details in %s)", detail, file)
				}
				res.Notes = append(res.Notes, "server reports a different order, see "+file)
			}
		}
	}
	return res, nil
}

// diagnose explains how the server's list differs from what we sent.
func diagnose(want, got []abp.PlaylistItem) (kind, detail string) {
	wantSet, gotSet := map[string]bool{}, map[string]bool{}
	for _, it := range want {
		wantSet[key(it)] = true
	}
	for _, it := range got {
		gotSet[key(it)] = true
	}
	missing, extra := 0, 0
	for k := range wantSet {
		if !gotSet[k] {
			missing++
		}
	}
	for k := range gotSet {
		if !wantSet[k] {
			extra++
		}
	}
	if missing > 0 || extra > 0 || len(want) != len(got) {
		return "members", fmt.Sprintf("%d expected item(s) missing, %d unexpected item(s) present (expected %d, server has %d)",
			missing, extra, len(want), len(got))
	}
	for i := range want {
		if key(want[i]) != key(got[i]) {
			return "order", fmt.Sprintf("same %d items but first difference at position %d: expected %s, server has %s",
				len(want), i+1, key(want[i]), key(got[i]))
		}
	}
	return "order", "orders differ"
}

// writeDiag saves a side-by-side of expected vs server order for troubleshooting.
func writeDiag(pl config.Playlist, want []Item, got []abp.PlaylistItem, summary string) string {
	path := filepath.Join(config.DataDir(), "mismatch-"+pl.ID+".txt")
	var b strings.Builder
	fmt.Fprintf(&b, "abp mismatch report for %q at %s\n%s\n\n", pl.ID, time.Now().Format(time.RFC3339), summary)
	fmt.Fprintf(&b, "%-4s  %-44s  %s\n", "pos", "expected (libraryItemId/episodeId)", "server has")
	n := len(want)
	if len(got) > n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		w, g, title := "-", "-", ""
		if i < len(want) {
			w = want[i].LibraryItemID + "/" + want[i].EpisodeID
			title = want[i].Podcast + " – " + want[i].Title
		}
		if i < len(got) {
			g = key(got[i])
		}
		mark := " "
		if w != g {
			mark = "!"
		}
		fmt.Fprintf(&b, "%-4d%s %-44s  %-44s  %s\n", i+1, mark, w, g, title)
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
	return path
}

// sameOrder reports whether a and b contain the same entries in the same order.
func sameOrder(a, b []abp.PlaylistItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if key(a[i]) != key(b[i]) {
			return false
		}
	}
	return true
}

// find locates the playlist on the server: by remembered id, else by name.
func find(ctx context.Context, c *abp.Client, pl config.Playlist, st *config.State) (*abp.Playlist, error) {
	all, err := c.Playlists(ctx)
	if err != nil {
		return nil, err
	}
	id := st.Get(pl.ID).ABSID
	if id != "" {
		for i := range all {
			if all[i].ID == id {
				return hydrate(ctx, c, all[i])
			}
		}
	}
	for i := range all {
		if all[i].LibraryID == pl.Library && strings.EqualFold(all[i].Name, pl.Name) {
			return hydrate(ctx, c, all[i])
		}
	}
	return nil, nil
}

// hydrate re-reads one playlist so we see its complete, ordered items (list
// endpoints may omit or abbreviate them).
func hydrate(ctx context.Context, c *abp.Client, p abp.Playlist) (*abp.Playlist, error) {
	full, err := c.GetPlaylist(ctx, p.ID)
	if err != nil {
		if errors.Is(err, abp.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if full.ID == "" {
		full.ID = p.ID
	}
	return full, nil
}

// DeleteRemote removes the Audiobookshelf playlist belonging to pl, if any.
func DeleteRemote(ctx context.Context, c *abp.Client, pl config.Playlist, st *config.State) error {
	p, err := find(ctx, c, pl, st)
	if err != nil {
		return err
	}
	if p != nil {
		if err := c.DeletePlaylist(ctx, p.ID); err != nil {
			return err
		}
	}
	st.Forget(pl.ID)
	return nil
}
