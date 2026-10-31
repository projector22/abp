// Package tui is the Bubble Tea interface for abp.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/projector22/abp/internal/abp"
	"github.com/projector22/abp/internal/config"
	"github.com/projector22/abp/internal/cron"
	"github.com/projector22/abp/internal/engine"
	"github.com/projector22/abp/internal/version"
)

// app is shared, mutable state that every screen can reach.
type app struct {
	cfg    *config.Config
	state  *config.State
	creds  config.Credentials
	client *abp.Client

	libs     []abp.Library
	podcasts map[string][]abp.LibraryItem // library id -> podcasts (cache)

	managed map[string]string // crontab: playlist id -> schedule
	cronErr error

	// remaining caches, per playlist id, how many unfinished episodes its
	// podcasts hold in total (the "to go" column on the list). It is loaded in
	// the background and refreshed now and then; screens only ever read it.
	remaining  map[string]engine.Remaining
	remLoading bool // a load is in flight
	remDirty   bool // a refresh was asked for while loading; run again when done

	build  version.Build  // which abp this is (shown in the header)
	update version.Result // latest update-check answer; zero value until it arrives
}

// refreshCron re-reads abp's entries from the crontab and caches the result (and
// any error) so screens can show whether each schedule is really installed
// without shelling out to crontab on every redraw.
func (a *app) refreshCron() {
	a.managed, a.cronErr = cron.Managed()
}

// connect adopts new credentials: it builds the API client and discards caches
// (libraries, podcast lists) that belonged to the previous server.
func (a *app) connect(c config.Credentials) {
	a.creds = c
	a.client = abp.NewClient(c.URL, c.APIKey)
	a.libs = nil
	a.podcasts = map[string][]abp.LibraryItem{}
}

// syncCron rewrites the crontab from the config and returns a status command.
func (a *app) syncCron() error {
	err := cron.Sync(a.cfg)
	a.refreshCron()
	return err
}

// ---- screens ---------------------------------------------------------------

type screen interface {
	Init() tea.Cmd
	Update(tea.Msg) (screen, tea.Cmd)
	View(w, h int) string
	Title() string
	Help() string
}

type (
	pushMsg    struct{ s screen }
	replaceMsg struct{ s screen }
	popMsg     struct{}
	statusMsg  struct {
		text string
		err  bool
	}
	busyMsg     struct{ text string }
	syncDoneMsg struct {
		name string
		res  *engine.Result
		err  error
	}
	syncAllDoneMsg struct{ ok, failed int }

	// refreshTickMsg fires every refreshEvery while the TUI is open. It makes the
	// screens pick up changes made by other abp processes (cron runs) and keeps
	// relative times such as "12m ago" current.
	refreshTickMsg struct{}

	// remainingMsg carries the unfinished-episode counts for every playlist.
	remainingMsg struct {
		res map[string]engine.Remaining
		err error
	}

	// cronRefreshedMsg carries a fresh reading of abp's crontab entries.
	cronRefreshedMsg struct {
		managed map[string]string
		err     error
	}

	// updateMsg carries the result of an update check. manual is true when the
	// user asked for it (v key), in which case the answer is also shown in the
	// status line.
	updateMsg struct {
		res    version.Result
		manual bool
	}
)

// push returns a command that navigates to s, stacking it on top of the current
// screen so that esc can return.
func push(s screen) tea.Cmd { return func() tea.Msg { return pushMsg{s} } }

// replace returns a command that swaps the current screen for s without adding
// to the navigation stack, e.g. moving from the setup screen to the list.
func replace(s screen) tea.Cmd { return func() tea.Msg { return replaceMsg{s} } }

// pop returns a command that closes the current screen and returns to the
// previous one.
func pop() tea.Cmd { return func() tea.Msg { return popMsg{} } }

// status returns a command that shows an informational message (printf-style) in
// the status line until the next key press.
func status(format string, args ...any) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: fmt.Sprintf(format, args...)} }
}

// statusErr is like status but renders the message as an error.
func statusErr(format string, args ...any) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: fmt.Sprintf(format, args...), err: true} }
}

// busy shows text with a spinner in the status line and runs cmd alongside. The
// spinner stays until something sends a busyMsg with empty text (the handlers
// for the work's result messages do this).
func busy(text string, cmd tea.Cmd) tea.Cmd {
	return tea.Batch(func() tea.Msg { return busyMsg{text} }, cmd)
}

// syncCmd syncs one playlist in the background and reports back with a
// syncDoneMsg. The playlist is copied first so the goroutine never touches
// config that the UI may be editing meanwhile.
func (a *app) syncCmd(id string) tea.Cmd {
	p := a.cfg.Find(id)
	if p == nil {
		return nil
	}
	pl := *p
	client := a.client
	return busy("Syncing "+pl.Name+"…", func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		res, err := engine.Run(ctx, client, pl, a.state, false)
		return syncDoneMsg{name: pl.Name, res: res, err: err}
	})
}

// remainingEvery is how many refresh ticks pass between reloads of the
// unfinished-episode counts. They cost one request per podcast, so this is much
// rarer than the state-file refresh.
const remainingEvery = 12

// remainingCmd reloads, in the background, the number of unfinished episodes of
// every playlist and reports back with a remainingMsg. If a load is already
// running it asks for another when that one finishes, so a change made meanwhile
// (e.g. adding a podcast) is never missed. It does nothing without a server
// connection or playlists. The podcast ids are copied so the goroutine never
// touches config that the UI may be editing meanwhile.
func (a *app) remainingCmd() tea.Cmd {
	if a.client == nil || len(a.cfg.Playlists) == 0 {
		return nil
	}
	if a.remLoading {
		a.remDirty = true
		return nil
	}
	a.remLoading = true
	pods := make(map[string][]string, len(a.cfg.Playlists))
	for _, p := range a.cfg.Playlists {
		ids := make([]string, len(p.Podcasts))
		for i, pc := range p.Podcasts {
			ids[i] = pc.ID
		}
		pods[p.ID] = ids
	}
	client := a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		res, err := engine.CountUnfinished(ctx, client, pods)
		return remainingMsg{res, err}
	}
}

// syncAllCmd syncs every playlist one after another in the background and
// reports the success/failure counts in a syncAllDoneMsg. Details of individual
// failures are stored in each playlist's state and shown on its screens.
func (a *app) syncAllCmd() tea.Cmd {
	pls := append([]config.Playlist(nil), a.cfg.Playlists...)
	client := a.client
	return busy("Syncing all playlists…", func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ok, failed := 0, 0
		for _, pl := range pls {
			if _, err := engine.Run(ctx, client, pl, a.state, false); err != nil {
				failed++
			} else {
				ok++
			}
		}
		return syncAllDoneMsg{ok, failed}
	})
}

// deletePlaylist removes a playlist from the config (and optionally from
// Audiobookshelf), then re-syncs the crontab.
func (a *app) deletePlaylist(id string, remote bool) tea.Cmd {
	p := a.cfg.Find(id)
	if p == nil {
		return nil
	}
	pl := *p
	a.cfg.Remove(id)
	if err := config.Save(a.cfg); err != nil {
		return statusErr("Could not save config: %v", err)
	}
	cronErr := a.syncCron()
	client := a.client
	return func() tea.Msg {
		msg := fmt.Sprintf("Deleted %q", pl.Name)
		if remote {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := engine.DeleteRemote(ctx, client, pl, a.state); err != nil {
				return statusMsg{text: msg + " locally, but Audiobookshelf said: " + err.Error(), err: true}
			}
			msg += " (also removed from Audiobookshelf)"
		} else {
			a.state.Forget(pl.ID)
		}
		if cronErr != nil {
			return statusMsg{text: msg + "; crontab: " + cronErr.Error(), err: true}
		}
		return statusMsg{text: msg}
	}
}

// ---- confirm prompts -------------------------------------------------------

type confirmOpt struct {
	key, label string
	run        func() tea.Cmd
}

type confirm struct {
	prompt string
	opts   []confirmOpt
}

// handle processes a key press while a confirmation is showing. It returns the
// command to run and whether the prompt is finished: a matching option runs, esc
// cancels, and any other key is ignored (the prompt stays open).
func (c *confirm) handle(k tea.KeyMsg) (tea.Cmd, bool) {
	for _, o := range c.opts {
		if k.String() == o.key {
			return o.run(), true
		}
	}
	if k.String() == "esc" {
		return nil, true
	}
	return nil, false
}

// view renders the prompt as a bordered box listing the available keys.
func (c *confirm) view() string {
	var keys []string
	for _, o := range c.opts {
		keys = append(keys, sKey.Render(o.key)+" "+sMuted.Render(o.label))
	}
	keys = append(keys, sKey.Render("esc")+" "+sMuted.Render("cancel"))
	return sBoxWarn.Render(sWarn.Render(c.prompt) + "\n\n" + strings.Join(keys, sMuted.Render("   ")))
}

// cancelOpt returns a ready-made "n / no" option that does nothing, for prompts
// that need an explicit decline key as well as esc.
func cancelOpt() confirmOpt { return confirmOpt{"n", "no", func() tea.Cmd { return nil }} }

// ---- root model ------------------------------------------------------------

type root struct {
	a      *app
	stack  []screen
	w, h   int
	status statusMsg
	busy   string
	spin   spinner.Model

	spinning bool // a spinner tick is in flight (ticks only run while busy)
	ticks    int  // refresh ticks seen so far
}

// refreshEvery is how often an open TUI re-reads the state file and redraws.
const refreshEvery = 5 * time.Second

// cronEvery is how many refresh ticks pass between re-reads of the crontab,
// which is a (slightly) more expensive shell-out than reading a small file.
const cronEvery = 6

// refreshTick returns a command that delivers the next refreshTickMsg after
// refreshEvery. Each tick handler schedules the following one, so the refresh
// loop keeps running for as long as the TUI is open.
func refreshTick() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

// refreshCronCmd re-reads the crontab in the background.
func refreshCronCmd() tea.Cmd {
	return func() tea.Msg {
		m, err := cron.Managed()
		return cronRefreshedMsg{managed: m, err: err}
	}
}

// newRoot creates the root model with first as the initial screen and sets up
// the spinner used for background work.
func newRoot(a *app, first screen) *root {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(cAccent)
	return &root{a: a, stack: []screen{first}, spin: sp}
}

// top returns the screen currently being shown (the top of the stack).
func (r *root) top() screen { return r.stack[len(r.stack)-1] }

// Init starts the initial screen and the spinner animation.
func (r *root) Init() tea.Cmd {
	// The update check runs in the background so start-up is never delayed; the
	// header shows its answer when it arrives (usually from the daily cache).
	// (The spinner is not started here: it only animates while something is busy.)
	return tea.Batch(r.top().Init(), refreshTick(), r.a.checkUpdateCmd(version.CacheTTL, false))
}

// Update is the Bubble Tea update function for the whole program. It handles
// everything global (window size, ctrl+c, the spinner, navigation, status and
// busy messages, sync results) and forwards every other message to the screen on
// top of the stack, replacing it with whatever updated screen comes back.
func (r *root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// refresh is set by messages that may have changed what the playlists
	// contain (a finished sync); the unfinished counts are reloaded after them.
	refresh := false
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		r.w, r.h = m.Width, m.Height
		return r, nil
	case tea.KeyMsg:
		if m.String() == "ctrl+c" {
			return r, tea.Quit
		}
		r.status = statusMsg{}
	case spinner.TickMsg:
		// Keep animating only while busy; otherwise let the tick chain end so an idle
		// TUI does not redraw ten times a second for nothing.
		if r.busy == "" {
			r.spinning = false
			return r, nil
		}
		var cmd tea.Cmd
		r.spin, cmd = r.spin.Update(m)
		return r, cmd
	case refreshTickMsg:
		// Pick up anything other abp processes (cron runs) have written. Merely
		// handling this message makes Bubble Tea redraw, which also refreshes the
		// "12m ago" style ages.
		r.a.state.Reload()
		r.ticks++
		cmds := []tea.Cmd{refreshTick()}
		if r.ticks%cronEvery == 0 {
			cmds = append(cmds, refreshCronCmd())
		}
		if r.ticks%remainingEvery == 0 {
			cmds = append(cmds, r.a.remainingCmd())
		}
		return r, tea.Batch(cmds...)
	case remainingMsg:
		r.a.remLoading = false
		if m.err == nil {
			// Merge rather than replace: a playlist whose podcasts failed this time
			// keeps its last good number instead of flickering to "?".
			next := make(map[string]engine.Remaining, len(m.res))
			for id, v := range m.res {
				if old, ok := r.a.remaining[id]; ok && v.Err != nil && old.Err == nil {
					v = old
				}
				next[id] = v
			}
			r.a.remaining = next
		}
		if r.a.remDirty {
			r.a.remDirty = false
			return r, r.a.remainingCmd()
		}
		return r, nil
	case cronRefreshedMsg:
		r.a.managed, r.a.cronErr = m.managed, m.err
		return r, nil
	case pushMsg:
		r.stack = append(r.stack, m.s)
		return r, m.s.Init()
	case replaceMsg:
		r.stack[len(r.stack)-1] = m.s
		return r, m.s.Init()
	case popMsg:
		if len(r.stack) > 1 {
			r.stack = r.stack[:len(r.stack)-1]
		}
		// The screen we are returning to may follow podcast edits made on the one
		// we left, so bring the counts up to date.
		return r, r.a.remainingCmd()
	case statusMsg:
		r.status = m
		return r, nil
	case busyMsg:
		r.busy = m.text
		if m.text != "" && !r.spinning {
			r.spinning = true
			return r, r.spin.Tick // start the animation
		}
		return r, nil
	case updateMsg:
		r.a.update = m.res
		if m.manual {
			r.busy = ""
			r.status = statusMsg{text: m.res.Summary(), err: m.res.Err != nil && m.res.Latest == ""}
		}
		return r, nil
	case syncDoneMsg:
		refresh = true
		// A sync finished: stop the spinner and summarise. Warnings (e.g. a podcast
		// that no longer exists) are shown like errors so they aren't missed.
		r.busy = ""
		switch {
		case m.err != nil:
			r.status = statusMsg{text: fmt.Sprintf("%s: %v", m.name, m.err), err: true}
		case len(m.res.Warnings) > 0:
			r.status = statusMsg{text: fmt.Sprintf("%s: %s — %s", m.name, m.res.Summary(), m.res.Warnings[0]), err: true}
		default:
			r.status = statusMsg{text: fmt.Sprintf("%s: %s", m.name, m.res.Summary())}
		}
	case syncAllDoneMsg:
		refresh = true
		r.busy = ""
		r.status = statusMsg{text: fmt.Sprintf("Synced %d playlists", m.ok), err: m.failed > 0}
		if m.failed > 0 {
			r.status.text += fmt.Sprintf(", %d failed (see each playlist for details)", m.failed)
		}
	}

	// Anything not handled above belongs to the screen on top of the stack.
	s, cmd := r.top().Update(msg)
	r.stack[len(r.stack)-1] = s
	if refresh {
		cmd = tea.Batch(cmd, r.a.remainingCmd())
	}
	return r, cmd
}

// View draws the frame: a header with breadcrumbs and the server, the current
// screen's body padded to fill the available height, and a footer made of the
// status line (spinner, success or error) and the screen's key help.
func (r *root) View() string {
	if r.w == 0 {
		return ""
	}
	pad := lipgloss.NewStyle().Padding(0, 2)

	crumbs := make([]string, 0, len(r.stack))
	for _, s := range r.stack {
		crumbs = append(crumbs, s.Title())
	}
	left := sBadge.Render("abp") + " " + sCrumb.Render(strings.Join(crumbs, sMuted.Render("  ›  ")))
	right := r.headerRight(r.w - 2 - lipgloss.Width(left) - 2)
	gap := r.w - 2 - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	header := lipgloss.NewStyle().Padding(0, 1).Render(left+strings.Repeat(" ", gap)+right) + "\n" + rule(r.w)

	var st string
	switch {
	case r.busy != "":
		st = r.spin.View() + " " + sText.Render(r.busy)
	case r.status.text != "" && r.status.err:
		st = sErr.Render("✗ " + r.status.text)
	case r.status.text != "":
		st = sOK.Render("✓ ") + sText.Render(r.status.text)
	}
	// The status and key-hint lines can be longer than the terminal. Wrap them
	// (lipgloss wraps coloured text correctly) rather than truncating, which
	// would miscount the colour escape codes.
	footer := rule(r.w) + "\n" + pad.Width(r.w).Render(st) + "\n" + pad.Width(r.w).Render(r.top().Help())

	bodyH := r.h - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyH < 1 {
		bodyH = 1
	}
	body := pad.Height(bodyH).Render(r.top().View(r.w-4, bodyH))
	// Last line of defence: no line may ever be wider than the terminal, or the
	// terminal wraps it and the whole layout shifts down.
	return lipgloss.NewStyle().MaxWidth(r.w).Render(lipgloss.JoinVertical(lipgloss.Left, header, body, footer))
}

// Run starts the interactive TUI for the given build. It loads the config and
// credentials and opens the playlist list, or the connection screen when no
// credentials exist yet. It blocks until the user quits.
func Run(build version.Build) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	a := &app{cfg: cfg, state: config.LoadState(), podcasts: map[string][]abp.LibraryItem{}, build: build}
	a.refreshCron()

	var first screen
	if creds.Valid() {
		a.connect(creds)
		first = newList(a)
	} else {
		a.creds = creds
		first = newSetup(a, true)
	}
	_, err = tea.NewProgram(newRoot(a, first), tea.WithAltScreen()).Run()
	return err
}

// saveCfg writes the in-memory config to config.yaml. Screens call it after every
// change so edits are persisted immediately.
func saveCfg(a *app) error { return config.Save(a.cfg) }

// checkUpdateCmd asks (in the background) whether a newer release exists and
// reports back with an updateMsg. maxAge is how old a cached answer may be; the
// manual flag marks a check the user asked for.
func (a *app) checkUpdateCmd(maxAge time.Duration, manual bool) tea.Cmd {
	build := a.build
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		return updateMsg{res: version.Check(ctx, build, maxAge), manual: manual}
	}
}

// versionCmd is the v key: re-check right now (ignoring the cache) and show the
// answer in the status line.
func (a *app) versionCmd() tea.Cmd {
	return busy("Checking for updates…", a.checkUpdateCmd(0, true))
}

// headerRight renders the right-hand side of the header, in at most max columns:
// an "update available" notice, this build's version, and the server. If it does
// not all fit, parts are dropped, least important first (the server, then the
// update notice). Any part may be absent: before the first connection there is
// no server, and most of the time there is no update.
func (r *root) headerRight(max int) string {
	host := strings.TrimPrefix(strings.TrimPrefix(r.a.creds.URL, "https://"), "http://")

	var upd, srv string
	if r.a.update.Status == version.UpdateAvailable {
		upd = sWarn.Render("↑ " + r.a.update.Latest + " available")
	}
	if host != "" {
		srv = sMuted.Render(trunc(host, 30))
	}
	ver := sMuted.Render(r.a.build.Short())

	join := func(parts ...string) string {
		var keep []string
		for _, p := range parts {
			if p != "" {
				keep = append(keep, p)
			}
		}
		return strings.Join(keep, sMuted.Render("  ·  "))
	}
	for _, cand := range []string{join(upd, ver, srv), join(upd, ver), join(ver, srv), ver} {
		if lipgloss.Width(cand) <= max {
			return cand
		}
	}
	return ""
}
