package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/projector22/abp/internal/abp"
	"github.com/projector22/abp/internal/config"
)

// fakeABS reproduces the real Audiobookshelf playlist semantics (read from its
// PlaylistController): batch/remove leaves gaps in the order numbers, batch/add
// numbers new items count+1, PATCH only reorders an identical set of items (400
// otherwise) and renumbers 1..n, removing the last item deletes the playlist,
// and the list endpoint omits items. Ties are broken adversarially.
type fakeABS struct {
	mu       sync.Mutex
	finished map[string]bool // "item/episode"
	pl       *fplaylist
	creates  int
	writes   int
	reverse  bool // report items in reverse order on GET (order-quirk simulation)
}

type fitem struct {
	it    abp.PlaylistItem
	order int
}

type fplaylist struct {
	id, lib, name, desc string
	items               []fitem
}

// sorted returns the playlist's items in the order the fake server would report
// them: by order number, with ties broken adversarially so tests catch code that
// relies on tied numbers resolving in any particular way.
func (p *fplaylist) sorted() []abp.PlaylistItem {
	cp := append([]fitem(nil), p.items...)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].order != cp[j].order {
			return cp[i].order < cp[j].order
		}
		return key(cp[i].it) > key(cp[j].it) // adversarial tie-break
	})
	out := make([]abp.PlaylistItem, len(cp))
	for i, f := range cp {
		out[i] = f.it
	}
	return out
}

// json converts the fake playlist to the API shape. The list endpoint omits
// items (withItems=false), and reverse simulates a server that reports a
// different order than the one stored.
func (p *fplaylist) json(reverse bool, withItems bool) abp.Playlist {
	out := abp.Playlist{ID: p.id, LibraryID: p.lib, Name: p.name, Description: p.desc}
	if withItems {
		out.Items = p.sorted()
		if reverse {
			for i, j := 0, len(out.Items)-1; i < j; i, j = i+1, j-1 {
				out.Items[i], out.Items[j] = out.Items[j], out.Items[i]
			}
		}
	}
	return out
}

// ep builds one episode in the JSON shape returned by the item endpoint.
func ep(id, title string, pub int64) map[string]any {
	return map[string]any{"id": id, "title": title, "publishedAt": pub, "addedAt": 1, "duration": 600}
}

// handler returns an HTTP handler that imitates the parts of the Audiobookshelf
// API abp uses, including its quirks: removal leaves numbering gaps, additions
// are numbered count+1, PATCH only reorders an identical set of items (400
// otherwise) and renumbers 1..n, and removing the last item deletes the
// playlist.
func (f *fakeABS) handler() http.Handler {
	items := map[string][]map[string]any{
		"podA": {ep("a1", "A1", 1000), ep("a2", "A2", 3000), ep("a3", "A3", 5000)},
		"podB": {ep("b1", "B1", 2000), ep("b2", "B2", 4000)},
		"podC": {ep("c0", "C0", 500)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var prog []map[string]any
		for k, v := range f.finished {
			if v {
				p := strings.Split(k, "/")
				prog = append(prog, map[string]any{"libraryItemId": p[0], "episodeId": p[1], "isFinished": true})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"username": "t", "mediaProgress": prog})
	})
	mux.HandleFunc("/api/items/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/items/")
		eps, ok := items[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "media": map[string]any{
			"metadata": map[string]any{"title": "Pod " + id}, "episodes": eps}})
	})
	mux.HandleFunc("/api/playlists", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == "POST" {
			var b struct {
				LibraryID   string             `json:"libraryId"`
				Name        string             `json:"name"`
				Description string             `json:"description"`
				Items       []abp.PlaylistItem `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.pl = &fplaylist{id: "pl1", lib: b.LibraryID, name: b.Name, desc: b.Description}
			for i, it := range b.Items {
				f.pl.items = append(f.pl.items, fitem{it, i + 1})
			}
			f.creates++
			f.writes++
			_ = json.NewEncoder(w).Encode(f.pl.json(false, true))
			return
		}
		out := []abp.Playlist{}
		if f.pl != nil {
			out = append(out, f.pl.json(false, false)) // list endpoint: no items
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"playlists": out})
	})
	mux.HandleFunc("/api/playlists/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, "/api/playlists/")
		id, action, _ := strings.Cut(rest, "/")
		if f.pl == nil || f.pl.id != id {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Name        string             `json:"name"`
			Description string             `json:"description"`
			Items       []abp.PlaylistItem `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p := f.pl
		switch {
		case r.Method == "PATCH":
			f.writes++
			if body.Name != "" {
				p.name = body.Name
			}
			if body.Description != "" { // server ignores an empty description
				p.desc = body.Description
			}
			if len(body.Items) > 0 {
				if len(body.Items) != len(p.items) {
					http.Error(w, "Invalid playlist items. Length mismatch", 400)
					return
				}
				idx := map[string]int{}
				for i, it := range body.Items {
					idx[key(it)] = i + 1
				}
				for i := range p.items {
					n, ok := idx[key(p.items[i].it)]
					if !ok {
						http.Error(w, "Invalid playlist items. Items not found", 400)
						return
					}
					p.items[i].order = n
				}
			}
		case action == "batch/add":
			f.writes++
			have := map[string]bool{}
			for _, f := range p.items {
				have[key(f.it)] = true
			}
			order := len(p.items) + 1
			for _, it := range body.Items {
				if !have[key(it)] {
					p.items = append(p.items, fitem{it, order})
					order++
				}
			}
		case action == "batch/remove":
			f.writes++
			rm := map[string]bool{}
			for _, it := range body.Items {
				rm[key(it)] = true
			}
			var left []fitem
			for _, f := range p.items {
				if !rm[key(f.it)] {
					left = append(left, f) // NOTE: no renumbering
				}
			}
			p.items = left
			if len(left) == 0 {
				f.pl = nil
				_, _ = w.Write([]byte("{}"))
				return
			}
		}
		_ = json.NewEncoder(w).Encode(p.json(f.reverse && r.Method == "GET", true))
	})
	return mux
}

// ids renders a list of playlist entries as comma-separated episode ids, which
// keeps test assertions short and readable.
func ids(items []abp.PlaylistItem) string {
	var s []string
	for _, i := range items {
		s = append(s, i.EpisodeID)
	}
	return strings.Join(s, ",")
}

// newEnv starts the fake server, points the data directory at a temp dir and
// returns a client and state ready for engine.Run.
func newEnv(t *testing.T, f *fakeABS, handler http.Handler) (*abp.Client, *config.State) {
	t.Setenv("ABP_DATA_DIR", t.TempDir())
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return abp.NewClient(srv.URL, "key"), config.LoadState()
}

// TestRunMaintainsPlaylistInPlace walks a playlist through its life: dry run,
// creation, no-op, finishing episodes from the front, adding an older podcast,
// a full reset, metadata changes, and being emptied then recreated. At each
// step the order must be exactly oldest-first and the same server playlist must
// be edited rather than recreated.
func TestRunMaintainsPlaylistInPlace(t *testing.T) {
	f := &fakeABS{finished: map[string]bool{}}
	c, st := newEnv(t, f, f.handler())
	pl := config.Playlist{ID: "x", Name: "Mix", Library: "lib", Limit: 4,
		Podcasts: []config.Podcast{{ID: "podA", Title: "A"}, {ID: "podB", Title: "B"}, {ID: "gone", Title: "Gone"}}}
	ctx := context.Background()
	got := func() string {
		if f.pl == nil {
			return "<none>"
		}
		return ids(f.pl.sorted())
	}
	run := func() *Result {
		t.Helper()
		res, err := Run(ctx, c, pl, st, false)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res
	}

	// dry run writes nothing
	if _, err := Run(ctx, c, pl, st, true); err != nil || f.writes != 0 {
		t.Fatalf("dry run: err=%v writes=%d", err, f.writes)
	}

	// create: oldest-first across podcasts, truncated to limit; missing podcast = warning
	res := run()
	if got() != "a1,b1,a2,b2" || res.Action != "created" || res.Unfinished != 5 || len(res.Warnings) != 1 {
		t.Fatalf("create: %s %+v", got(), res)
	}

	// no change -> no writes
	w := f.writes
	if res = run(); res.Action != "unchanged" || f.writes != w {
		t.Fatalf("expected no-op, got %+v", res)
	}

	// finish a1 (front removal) -> a3 joins at the END, not the middle. This is the
	// case that broke against the real server: removal leaves a numbering gap.
	f.finished["podA/a1"] = true
	run()
	if got() != "b1,a2,b2,a3" || f.creates != 1 {
		t.Fatalf("after finishing a1: %s creates=%d", got(), f.creates)
	}

	// finish two more at once, still correct
	f.finished["podB/b1"] = true
	f.finished["podA/a2"] = true
	run()
	if got() != "b2,a3" {
		t.Fatalf("after finishing b1,a2: %s", got())
	}

	// keep finishing/adding over many rounds: order must never drift
	f.finished["podB/b2"] = true
	run()
	if got() != "a3" {
		t.Fatalf("got %s", got())
	}

	// add a podcast whose episode is older than everything: whole order shifts,
	// playlist never emptied/recreated
	pl.Podcasts = append(pl.Podcasts, config.Podcast{ID: "podC", Title: "C"})
	run()
	if got() != "c0,a3" || f.creates != 1 {
		t.Fatalf("after adding podC: %s creates=%d", got(), f.creates)
	}

	// everything back to unfinished: full reshuffle with item additions
	f.finished = map[string]bool{}
	run()
	if got() != "c0,a1,b1,a2" {
		t.Fatalf("after reset: %s", got())
	}
	if res = run(); res.Action != "unchanged" {
		t.Fatalf("should settle, got %+v", res)
	}

	// rename + description
	pl.Name, pl.Description = "Renamed", "Slow-burn history"
	run()
	if f.pl.name != "Renamed" || f.pl.desc != "Slow-burn history" || got() != "c0,a1,b1,a2" {
		t.Fatalf("metadata update wrong: %+v", f.pl)
	}
	w = f.writes
	if res = run(); res.Action != "unchanged" || f.writes != w {
		t.Fatal("should be in sync now")
	}

	// clearing the description can't be done via the API; must not cause endless updates
	pl.Description = ""
	if res = run(); res.Action != "unchanged" {
		t.Fatalf("empty description should be ignored, got %+v", res)
	}

	// everything finished -> ABS drops the playlist; new episodes bring it back
	for _, k := range []string{"podA/a1", "podA/a2", "podA/a3", "podB/b1", "podB/b2", "podC/c0"} {
		f.finished[k] = true
	}
	if res = run(); res.Action != "emptied" || f.pl != nil {
		t.Fatalf("emptied: %+v", res)
	}
	delete(f.finished, "podA/a2")
	if res = run(); res.Action != "created" || got() != "a2" || f.creates != 2 {
		t.Fatalf("recreate: %s %+v", got(), res)
	}
}

// A playlist already left scrambled by the old behaviour must be repaired.
func TestRunRepairsScrambledPlaylist(t *testing.T) {
	f := &fakeABS{finished: map[string]bool{}}
	c, st := newEnv(t, f, f.handler())
	pl := config.Playlist{ID: "x", Name: "Mix", Library: "lib", Limit: 5,
		Podcasts: []config.Podcast{{ID: "podA"}, {ID: "podB"}}}
	f.pl = &fplaylist{id: "pl1", lib: "lib", name: "Mix", items: []fitem{
		{abp.PlaylistItem{LibraryItemID: "podA", EpisodeID: "a3"}, 2},
		{abp.PlaylistItem{LibraryItemID: "podB", EpisodeID: "b1"}, 2}, // tie + gap
		{abp.PlaylistItem{LibraryItemID: "podA", EpisodeID: "a1"}, 7},
		{abp.PlaylistItem{LibraryItemID: "podB", EpisodeID: "b2"}, 3},
		{abp.PlaylistItem{LibraryItemID: "podA", EpisodeID: "a2"}, 5},
	}}
	if _, err := Run(context.Background(), c, pl, st, false); err != nil {
		t.Fatal(err)
	}
	if got := ids(f.pl.sorted()); got != "a1,b1,a2,b2,a3" {
		t.Fatalf("not repaired: %s", got)
	}
	for _, it := range f.pl.items { // renumbered 1..n with no gaps/ties
		if it.order < 1 || it.order > 5 {
			t.Fatalf("orders not tidy: %+v", f.pl.items)
		}
	}
}

// A server that silently ignores our writes must produce an error, never a
// false "updated".
func TestRunDetectsServerNotApplyingChanges(t *testing.T) {
	f := &fakeABS{finished: map[string]bool{}}
	inner := f.handler()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && strings.HasPrefix(r.URL.Path, "/api/playlists/") {
			_, _ = w.Write([]byte("{}")) // 200 OK but does nothing
			return
		}
		inner.ServeHTTP(w, r)
	})
	c, st := newEnv(t, f, h)
	pl := config.Playlist{ID: "x", Name: "Mix", Library: "lib", Limit: 4, Podcasts: []config.Podcast{{ID: "podA"}, {ID: "podB"}}}
	ctx := context.Background()
	if _, err := Run(ctx, c, pl, st, false); err != nil {
		t.Fatal(err)
	}
	f.finished["podA/a1"] = true
	if _, err := Run(ctx, c, pl, st, false); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected verification error, got %v", err)
	}
}

// If the server holds the right episodes but reports them in another order,
// sync succeeds with a note and leaves a report file, rather than failing.
func TestRunOrderOnlyMismatchIsANoteNotAFailure(t *testing.T) {
	f := &fakeABS{finished: map[string]bool{}}
	c, st := newEnv(t, f, f.handler())
	dir := os.Getenv("ABP_DATA_DIR")
	pl := config.Playlist{ID: "x", Name: "Mix", Library: "lib", Limit: 4, Podcasts: []config.Podcast{{ID: "podA"}, {ID: "podB"}}}
	ctx := context.Background()
	if _, err := Run(ctx, c, pl, st, false); err != nil {
		t.Fatal(err)
	}
	f.reverse = true
	f.finished["podA/a1"] = true
	res, err := Run(ctx, c, pl, st, false)
	if err != nil {
		t.Fatalf("order-only mismatch must not fail: %v", err)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Summary(), "different order") {
		t.Fatalf("expected an order note, got %+v", res.Notes)
	}
	if _, err := os.Stat(filepath.Join(dir, "mismatch-x.txt")); err != nil {
		t.Fatalf("expected mismatch report: %v", err)
	}
}

// TestCountUnfinished checks the per-playlist "to go" totals: they count every
// unfinished episode (not just those within the limit), agree with BuildQueue,
// ignore finished episodes, skip a deleted podcast, and count a podcast shared
// by two playlists for each of them.
func TestCountUnfinished(t *testing.T) {
	f := &fakeABS{finished: map[string]bool{"podA/a1": true}}
	c, _ := newEnv(t, f, f.handler())
	ctx := context.Background()

	got, err := CountUnfinished(ctx, c, map[string][]string{
		"mix":   {"podA", "podB", "gone"},
		"onlyA": {"podA"},
		"none":  nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"mix": 4, "onlyA": 2, "none": 0} {
		if got[id].Err != nil || got[id].Unfinished != want {
			t.Errorf("%s: got %+v, want %d unfinished", id, got[id], want)
		}
	}

	// It must match what the preview/sync reports as the unfinished total, even
	// when the playlist limit is far smaller.
	pl := config.Playlist{ID: "mix", Limit: 1, Podcasts: []config.Podcast{{ID: "podA"}, {ID: "podB"}, {ID: "gone"}}}
	q, err := BuildQueue(ctx, c, pl)
	if err != nil {
		t.Fatal(err)
	}
	if q.Unfinished != got["mix"].Unfinished {
		t.Errorf("BuildQueue says %d unfinished, CountUnfinished says %d", q.Unfinished, got["mix"].Unfinished)
	}
}

// TestCountUnfinishedPodcastError checks that a podcast failing with a real
// error (not "deleted") only marks the playlists that use it, and leaves the
// others with their counts.
func TestCountUnfinishedPodcastError(t *testing.T) {
	f := &fakeABS{finished: map[string]bool{}}
	inner := f.handler()
	broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/podB") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	})
	c, _ := newEnv(t, f, broken)

	got, err := CountUnfinished(context.Background(), c, map[string][]string{
		"ok":  {"podA"},
		"bad": {"podA", "podB"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["ok"].Err != nil || got["ok"].Unfinished != 3 {
		t.Errorf("ok playlist: %+v", got["ok"])
	}
	if got["bad"].Err == nil {
		t.Errorf("bad playlist should carry the podcast's error: %+v", got["bad"])
	}
}
