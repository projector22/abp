package tui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projector22/abp/internal/abp"
	"github.com/projector22/abp/internal/config"
	"github.com/projector22/abp/internal/engine"
)

// errTest stands in for any failure of the whole load.
var errTest = errors.New("server unreachable")

// fakeServer imitates the two Audiobookshelf endpoints the "to go" count uses:
// /api/me (here: episode a1 of podA is finished) and /api/items/<id>, which
// serves podA with three episodes and podB with two. Any other id is a 404.
func fakeServer(t *testing.T) *abp.Client {
	t.Helper()
	pods := map[string][]string{"podA": {"a1", "a2", "a3"}, "podB": {"b1", "b2"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"mediaProgress": []map[string]any{
			{"libraryItemId": "podA", "episodeId": "a1", "isFinished": true},
		}})
	})
	mux.HandleFunc("/api/items/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/items/")
		ids, ok := pods[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var eps []map[string]any
		for _, e := range ids {
			eps = append(eps, map[string]any{"id": e, "title": e, "publishedAt": 1000})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "media": map[string]any{
			"metadata": map[string]any{"title": id}, "episodes": eps}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return abp.NewClient(srv.URL, "key")
}

// newCountApp builds an app with one playlist over podA and podB (plus a podcast
// the server has deleted) whose server is the fake above.
func newCountApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("ABP_DATA_DIR", t.TempDir())
	t.Setenv("ABP_CONFIG", t.TempDir()+"/config.yaml")
	return &app{
		cfg: &config.Config{Playlists: []config.Playlist{{
			ID: "mix", Name: "Mix", Limit: 2,
			Podcasts: []config.Podcast{{ID: "podA"}, {ID: "podB"}, {ID: "gone"}},
		}}},
		state:    config.LoadState(),
		podcasts: map[string][]abp.LibraryItem{},
		managed:  map[string]string{},
		client:   fakeServer(t),
	}
}

// TestRemainingLoadsFromServer runs the background load end to end: opening the
// list starts it, the result fills the column (2 + 2 unfinished, though the limit
// is 2), and a refresh asked for mid-load is queued rather than lost or doubled.
func TestRemainingLoadsFromServer(t *testing.T) {
	a := newCountApp(t)
	l := newList(a)
	r := newRoot(a, l)
	r.Update(tea.WindowSizeMsg{Width: 130, Height: 24})

	cmd := l.Init()
	if cmd == nil || !a.remLoading {
		t.Fatal("opening the list should start a load")
	}
	// A second request while loading is queued, not run in parallel.
	if again := a.remainingCmd(); again != nil || !a.remDirty {
		t.Fatalf("second request: cmd=%v dirty=%v", again, a.remDirty)
	}

	msg, ok := cmd().(remainingMsg)
	if !ok || msg.err != nil {
		t.Fatalf("load returned %#v", msg)
	}
	_, next := r.Update(msg)
	// The queued request runs as soon as the first load ends, and is then spent.
	if next == nil || !a.remLoading || a.remDirty {
		t.Errorf("queued refresh: cmd=%v loading=%v dirty=%v", next, a.remLoading, a.remDirty)
	}
	if got := a.remaining["mix"]; got.Err != nil || got.Unfinished != 4 {
		t.Errorf("mix: got %+v, want 4 unfinished", got)
	}
	if !strings.Contains(r.View(), "4") {
		t.Errorf("the list should show 4 to go:\n%s", r.View())
	}
}

// TestRemainingRefreshTriggers checks the moments that reload the counts: the
// periodic tick (only every remainingEvery ticks), leaving a screen, and a
// finished sync. It also checks nothing happens without a server or playlists.
func TestRemainingRefreshTriggers(t *testing.T) {
	a := newCountApp(t)
	r := newRoot(a, newList(a))
	r.Update(tea.WindowSizeMsg{Width: 130, Height: 24})

	// Ticks before the interval do not reload; the remainingEvery-th does.
	for i := 1; i < remainingEvery; i++ {
		r.Update(refreshTickMsg{})
	}
	if a.remLoading {
		t.Fatal("no reload expected before the interval")
	}
	r.Update(refreshTickMsg{})
	if !a.remLoading {
		t.Fatal("the periodic tick should reload the counts")
	}

	// Going back to the list reloads too (here: queued, as one is in flight).
	r.Update(pushMsg{newDetail(a, "mix")})
	r.Update(popMsg{})
	if !a.remDirty {
		t.Error("returning to the list should ask for a reload")
	}

	// A finished sync reloads (the first sync-all message is the one that counts).
	a.remLoading, a.remDirty = false, false
	r.Update(syncAllDoneMsg{ok: 1})
	if !a.remLoading {
		t.Error("a finished sync should reload the counts")
	}

	// No connection, or nothing to count: no work.
	a.remLoading = false
	a.client = nil
	if a.remainingCmd() != nil {
		t.Error("without a client there is nothing to load")
	}
	a.client = fakeServer(t)
	a.cfg.Playlists = nil
	if a.remainingCmd() != nil {
		t.Error("without playlists there is nothing to load")
	}
}

// TestRemainingLoadFailureKeepsOldCounts checks a wholly failed load (the server
// is unreachable) changes nothing and clears the loading flag so it can retry.
func TestRemainingLoadFailureKeepsOldCounts(t *testing.T) {
	a := newCountApp(t)
	r := newRoot(a, newList(a))
	a.remLoading = true
	a.remaining = map[string]engine.Remaining{"mix": {Unfinished: 9}}
	r.Update(remainingMsg{err: errTest})
	if a.remLoading || a.remaining["mix"].Unfinished != 9 {
		t.Errorf("failed load: loading=%v remaining=%+v", a.remLoading, a.remaining)
	}
}
