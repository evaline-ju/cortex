package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
)

// clearConfirm is X's confirmation, modal while the model holds one. It asks before anything is
// erased, and says what will be: the counts come from ?archived=true, the proxy's own list of
// everything it holds, rather than from what agentop happens to have cached.
type clearConfirm struct {
	counted  bool
	list     apiclient.SessionList
	countErr error
	// clearing is y pressed, with the DELETE in flight.
	clearing bool
}

type clearCountsMsg struct {
	list apiclient.SessionList
	err  error
}

type clearDoneMsg struct {
	res apiclient.ClearResult
	err error
}

// openClearConfirm is X: put the confirmation up and count what it would erase.
func (m *model) openClearConfirm() tea.Cmd {
	m.clearConfirm = &clearConfirm{}
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		list, err := c.ListSessionsArchived(ctx)
		return clearCountsMsg{list: list, err: err}
	}
}

// clearConfirmKey owns the keyboard while the confirmation is up. y erases; n, N, esc, q and
// ctrl+c keep everything — cancelling is never the dangerous direction, so every key that reads
// as "get me out" does it. Every other key is swallowed, so nothing reaches the pane beneath.
func (m *model) clearConfirmKey(msg tea.KeyMsg) tea.Cmd {
	cc := m.clearConfirm
	if cc.clearing {
		// Already sent. esc only hides the dialog; the answer still lands as a flash.
		if msg.String() == "esc" {
			m.clearConfirm = nil
		}
		return nil
	}
	switch msg.String() {
	case "y", "Y":
		cc.clearing = true
		c, ctx := m.client, m.ctx
		return func() tea.Msg {
			res, err := c.ClearSessions(ctx)
			return clearDoneMsg{res: res, err: err}
		}
	case "n", "N", "esc", "q", "ctrl+c":
		m.clearConfirm = nil
	}
	return nil
}

// applyClearCounts fills in the confirmation's figures, unless it was dismissed meanwhile.
func (m *model) applyClearCounts(msg clearCountsMsg) {
	if m.clearConfirm == nil {
		return
	}
	m.clearConfirm.counted, m.clearConfirm.list, m.clearConfirm.countErr = true, msg.list, msg.err
}

// applyClearDone reports the clear, and on success drops what agentop held about the sessions it
// erased. Applied whether or not the confirmation is still up: esc while clearing only hid it.
func (m *model) applyClearDone(msg clearDoneMsg) tea.Cmd {
	m.clearConfirm = nil
	if msg.err != nil {
		if errors.Is(msg.err, apiclient.ErrClearUnsupported) {
			m.setFlash(msg.err.Error())
		} else {
			m.setFlash("nothing cleared: " + msg.err.Error())
		}
		return nil
	}
	m.resetAfterClear()
	r := msg.res
	switch {
	case r.ArchiveError != "":
		m.setFlash(fmt.Sprintf("cleared %d sessions from memory, but not from disk: %s", r.Sessions, r.ArchiveError))
	case r.ArchivedSessions > 0 || r.Bytes > 0:
		m.setFlash(fmt.Sprintf("cleared %d sessions, and %d (%s) from disk; the cost ledger is kept",
			r.Sessions, r.ArchivedSessions, formatBytes(r.Bytes)))
	default:
		m.setFlash(fmt.Sprintf("cleared %d sessions; the cost ledger is kept", r.Sessions))
	}
	return m.loadSessionsCmd()
}

// resetAfterClear drops everything agentop holds that describes a session, and keeps the
// connection, the operator's choices and the spend strip — whose figures the ledger keeps.
//
// backToPodsPane is the checklist, because it resets the same things for a different reason; the
// half kept here is everything that describes the connection rather than the sessions on it.
// clearGen is what keeps a snapshot answered before the clear from bringing a session back: see
// snapshotCmd.
func (m *model) resetAfterClear() {
	m.clearGen++
	m.sessions = nil
	m.events = make(map[string][]pipeline.SessionEvent)
	m.fullFetched = nil
	m.contextRun = nil
	m.olderNotFetched = nil
	m.paging = nil
	m.usage.snap = nil
	m.usage.err = nil
	m.usage.lastFetch = time.Time{}
	m.usage.reqSeq++
	m.untitledMisses = 0
	m.untitledCounted = nil
	m.archiveUsage = nil
	m.detailEvent = nil
	m.detailPlugin = nil
	m.selectedSess = ""
	m.selectedEventKey = eventKey{}
	m.visibleRows = nil
	m.pane = paneSessions
	m.rebuildSessionsTable()
}

// renderClearConfirm draws the confirmation. Returns the panel only; see overlayCenter.
func renderClearConfirm(cc *clearConfirm, width int) string {
	const wrap = 60
	w := min(wrap, max(width-6, 20))
	var b strings.Builder
	switch {
	case !cc.counted:
		b.WriteString("Counting this Cortex's sessions…")
	default:
		if cc.countErr != nil {
			b.WriteString("Could not count this Cortex's sessions: " + cc.countErr.Error() + "\n\n")
			b.WriteString("Erase every session from this Cortex, in memory and on disk?")
		} else if a := cc.list.Archive; a != nil {
			fmt.Fprintf(&b, "Erase all %d sessions from this Cortex, in memory and on disk (%s)?",
				len(cc.list.Sessions), formatBytes(a.Bytes))
		} else {
			fmt.Fprintf(&b, "Erase all %d sessions from this Cortex? It keeps no history on disk.",
				len(cc.list.Sessions))
		}
		b.WriteString("\n\nThe cost ledger is kept. This cannot be undone.")
	}
	hint := "[y] erase  [n] keep"
	if cc.clearing {
		hint = "erasing…"
	}
	body := lipgloss.NewStyle().Width(w).Render(b.String()) + "\n\n" + styleHint.Render(hint)
	box := styleBorder.Padding(0, 2)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(body)
}
