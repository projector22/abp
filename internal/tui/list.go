package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projector22/abp/internal/config"
	"github.com/projector22/abp/internal/cron"
)

type listScreen struct {
	a       *app
	cur     int
	confirm *confirm
}

// newList creates the main screen listing all playlists.
func newList(a *app) *listScreen { return &listScreen{a: a} }

// Title is the breadcrumb label shown in the header for this screen.
func (s *listScreen) Title() string { return "Playlists" }

// Init starts loading how many unfinished episodes each playlist has to go.
func (s *listScreen) Init() tea.Cmd { return s.a.remainingCmd() }

// Help returns the key hints for the footer (hidden while a confirmation is
// open, which shows its own keys).
func (s *listScreen) Help() string {
	if s.confirm != nil {
		return ""
	}
	return helpLine("↑↓", "move", "enter", "open", "n", "new", "d", "delete", "s", "sync", "S", "sync all", "C", "crontab", "o", "server", "v", "version", "q", "quit")
}

// Update handles keys on the playlist list: navigation, opening, creating and
// deleting playlists, syncing one or all, reinstalling the crontab, opening the
// server settings, checking for a new version and quitting.
func (s *listScreen) Update(msg tea.Msg) (screen, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if s.confirm != nil {
		cmd, done := s.confirm.handle(k)
		if done {
			s.confirm = nil
		}
		return s, cmd
	}
	pls := s.a.cfg.Playlists
	n := len(pls)
	s.cur = clamp(s.cur, 0, n-1)

	switch k.String() {
	case "up", "k":
		s.cur = clamp(s.cur-1, 0, n-1)
	case "down", "j":
		s.cur = clamp(s.cur+1, 0, n-1)
	case "enter", "right", "l":
		if n > 0 {
			return s, push(newDetail(s.a, pls[s.cur].ID))
		}
	case "n":
		return s, push(newForm(s.a, "", false))
	case "d":
		if n > 0 {
			id, name := pls[s.cur].ID, pls[s.cur].Name
			s.confirm = &confirm{
				prompt: fmt.Sprintf("Delete playlist %q?", name),
				opts: []confirmOpt{
					{"y", "delete + remove from Audiobookshelf", func() tea.Cmd { return s.a.deletePlaylist(id, true) }},
					{"k", "delete, keep the Audiobookshelf playlist", func() tea.Cmd { return s.a.deletePlaylist(id, false) }},
				},
			}
		}
	case "s":
		if n > 0 {
			return s, s.a.syncCmd(pls[s.cur].ID)
		}
	case "S":
		if n > 0 {
			return s, s.a.syncAllCmd()
		}
	case "C":
		if err := s.a.syncCron(); err != nil {
			return s, statusErr("crontab: %v", err)
		}
		return s, status("Crontab now matches your schedules")
	case "o":
		return s, push(newSetup(s.a, false))
	case "v":
		return s, s.a.versionCmd()
	case "q":
		return s, tea.Quit
	}
	return s, nil
}

// remainingBadge renders the "to go" column: the total number of unfinished
// episodes across the playlist's podcasts, not just the ones that fit in its
// limit. It shows "…" until the first load finishes, "?" if a podcast could not
// be read and nothing is known yet, and "0" muted when everything is finished.
func (a *app) remainingBadge(p config.Playlist) string {
	r, ok := a.remaining[p.ID]
	switch {
	case !ok:
		return sMuted.Render("…")
	case r.Err != nil:
		return sWarn.Render("?")
	case r.Unfinished == 0:
		return sMuted.Render("0")
	default:
		return sText.Render(fmt.Sprintf("%d", r.Unfinished))
	}
}

// scheduleBadge renders a playlist's schedule for the list: "manual" when it has
// none, a green dot when the schedule is installed in the crontab, and a hollow
// amber dot when it is configured but not (yet) installed.
func scheduleBadge(a *app, p config.Playlist) string {
	if p.Schedule == "" {
		return sMuted.Render("manual")
	}
	label := cron.Describe(p.Schedule)
	if a.managed[p.ID] == p.Schedule {
		return sOK.Render("● ") + sText.Render(label)
	}
	return sWarn.Render("○ ") + sText.Render(label)
}

// syncBadge renders the outcome of a playlist's last sync: failed, never run, or
// how long ago it succeeded.
func syncBadge(st config.PlaylistState) string {
	switch {
	case st.Error != "":
		return sErr.Render("✗ failed " + ago(st.LastSync))
	case st.LastSync.IsZero():
		return sMuted.Render("never synced")
	default:
		return sOK.Render("✓ ") + sMuted.Render(ago(st.LastSync))
	}
}

// View draws the playlist table (name, size, episodes to go, podcast count, schedule, last sync)
// with the selected row highlighted, the last result of the selected playlist,
// any crontab problem, and an open confirmation. With no playlists it shows a
// short welcome instead.
func (s *listScreen) View(w, h int) string {
	pls := s.a.cfg.Playlists
	if len(pls) == 0 {
		msg := sTitle.Render("No playlists yet") + "\n\n" +
			sText.Render("A smart playlist keeps itself filled with the oldest unfinished\n"+
				"episodes from the podcasts you pick, in publish order.") + "\n\n" +
			sMuted.Render("Press ") + sKey.Render("n") + sMuted.Render(" to create your first one.")
		return "\n" + sBox.Render(msg)
	}
	s.cur = clamp(s.cur, 0, len(pls)-1)

	const (
		wSize  = 9
		wLeft  = 12
		wPods  = 10
		wSched = 22
		wSync  = 18
		wRun   = 18
	)
	wName := w - 2 - wSize - wLeft - wPods - wSched - wSync - wRun
	if wName < 12 {
		wName = 12
	}

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("  " + sColHdr.Render(cell("PLAYLIST", wName)+cell("SIZE", wSize)+cell("TO GO", wLeft)+cell("PODCASTS", wPods)+cell("SCHEDULE", wSched)+cell("LAST SYNC", wSync)+"LAST RUN") + "\n")

	avail := h - 4
	if s.confirm != nil {
		avail -= 5
	}
	start, end := window(s.cur, len(pls), avail)
	for i := start; i < end; i++ {
		p := pls[i]
		st := s.a.state.Get(p.ID)
		cursor, name := "  ", sText.Render(cell(p.Name, wName))
		if i == s.cur {
			cursor = sSel.Render("▸ ")
			name = sSel.Render(cell(p.Name, wName))
		}
		sched := scheduleBadge(s.a, p)
		sched += strings.Repeat(" ", max(0, wSched-visibleWidth(sched)))
		b.WriteString(cursor + name +
			sText.Render(cell(fmt.Sprintf("%d eps", p.Limit), wSize)) +
			cell0(s.a.remainingBadge(p), wLeft) +
			sText.Render(cell(fmt.Sprintf("%d", len(p.Podcasts)), wPods)) +
			sched + cell0(syncBadge(st), wSync) + runBadge(s.a, p, st) + "\n")
	}
	if len(pls) > end-start {
		b.WriteString(sMuted.Render(fmt.Sprintf("\n  showing %d–%d of %d", start+1, end, len(pls))) + "\n")
	}
	if s.cur < len(pls) {
		sel := pls[s.cur]
		st := s.a.state.Get(sel.ID)
		if sel.Schedule != "" && st.ScheduledError != "" {
			b.WriteString("\n" + sErr.Render("  scheduled run failed: "+trunc(st.ScheduledError, w-26)) + "\n")
		}
		if st.Error != "" {
			b.WriteString("\n" + sErr.Render("  "+trunc(st.Error, w-4)) + "\n")
		} else if st.Summary != "" {
			b.WriteString("\n" + sMuted.Render("  last sync: "+st.Summary) + "\n")
		}
	}
	if s.a.cronErr != nil {
		b.WriteString("\n" + sWarn.Render("  crontab unavailable: "+trunc(s.a.cronErr.Error(), w-24)) + "\n")
	}
	if s.confirm != nil {
		b.WriteString("\n" + s.confirm.view())
	}
	return b.String()
}

// runBadge renders the "last run" column: how the most recent run started by
// the crontab went. It is blank ("—") for playlists with no schedule. A
// schedule that has never run says why it might not have: not yet, or because
// it isn't installed in the crontab at all.
func runBadge(a *app, p config.Playlist, st config.PlaylistState) string {
	switch {
	case p.Schedule == "":
		return sMuted.Render("—")
	case st.ScheduledError != "":
		return sErr.Render("✗ failed " + ago(st.LastScheduled))
	case !st.LastScheduled.IsZero():
		return sOK.Render("✓ ") + sMuted.Render(ago(st.LastScheduled))
	case a.managed[p.ID] != p.Schedule:
		return sWarn.Render("not installed")
	default:
		return sMuted.Render("not run yet")
	}
}

// cell0 pads an already-styled string with spaces to n display columns. (cell
// is for plain text: it truncates, which would miscount colour codes.)
func cell0(s string, n int) string {
	return s + strings.Repeat(" ", max(0, n-visibleWidth(s)))
}
