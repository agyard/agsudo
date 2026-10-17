package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/client"
	"github.com/charmbracelet/x/ansi"
)

type reviewItem struct{ request api.Request }

func (i reviewItem) Title() string { return i.request.State + "  " + commandLabel(i.request) }
func (i reviewItem) Description() string {
	reason := i.request.Spec.Reason
	if reason == "" {
		reason = "no reason given"
	}
	return prose(i.request.Spec.Target.Name + " · " + reason + " · " + i.request.ID)
}

// substringFilter keeps queue order; fuzzy matching surprised reviewers.
func substringFilter(term string, targets []string) []list.Rank {
	var ranks []list.Rank
	term = strings.ToLower(term)
	for i, target := range targets {
		if strings.Contains(strings.ToLower(target), term) {
			ranks = append(ranks, list.Rank{Index: i})
		}
	}
	return ranks
}
func (i reviewItem) FilterValue() string {
	return command(i.request) + " " + i.request.Spec.Reason + " " + i.request.Spec.Session + " " + i.request.ID
}

type reviewBackend interface {
	Call(context.Context, api.Input, any) error
}
type refreshed struct {
	requests   []api.Request
	err        error
	generation int
}
type decided struct {
	request api.Request
	err     error
}
type refreshTick struct{}
type reviewModel struct {
	ctx                context.Context
	backend            reviewBackend
	states             []string
	session            string
	queue              list.Model
	details            viewport.Model
	reason             textinput.Model
	width, height      int
	all, loading, busy bool
	generation         int
	selectedID         string
	confirm            *api.Request
	action, status     string
	errorStatus        bool
}

func runTUI(ctx context.Context, c client.Client, states []string, session string, in io.Reader, out io.Writer) error {
	if !isTerminal(in) || !isTerminal(out) {
		return api.Err("terminal_required", "review --tui requires an interactive terminal; agents use agsudo --json review")
	}
	uiCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(newReviewModel(uiCtx, c, states, session), tea.WithContext(uiCtx), tea.WithInput(in), tea.WithOutput(out)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func newReviewModel(ctx context.Context, b reviewBackend, states []string, session string) *reviewModel {
	d := list.NewDefaultDelegate()
	d.SetSpacing(0)
	l := list.New(nil, d, 44, 18)
	l.Title = "Approval queue"
	l.SetStatusBarItemName("request", "requests")
	l.DisableQuitKeybindings()
	l.SetShowHelp(false)
	l.Filter = substringFilter
	l.Styles.TitleBar = l.Styles.TitleBar.PaddingBottom(0)
	l.Styles.StatusBar = l.Styles.StatusBar.PaddingBottom(0)
	l.SetShowPagination(false) // The status bar already counts requests.
	v := viewport.New(viewport.WithWidth(60), viewport.WithHeight(18))
	input := textinput.New()
	input.Prompt = "Reason > "
	input.Placeholder = "optional · Enter confirms · Esc goes back"
	input.CharLimit = 2048
	input.SetWidth(65)
	m := &reviewModel{ctx: ctx, backend: b, states: states, session: session, queue: l, details: v, reason: input, width: 110, height: 30}
	m.layout()
	return m
}
func (m *reviewModel) Init() tea.Cmd { return tea.Batch(m.fetch(), tickReview()) }
func tickReview() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return refreshTick{} })
}
func (m *reviewModel) fetch() tea.Cmd {
	m.loading = true
	m.generation++
	gen, b, ctx, session := m.generation, m.backend, m.ctx, m.session
	states := append([]string(nil), m.states...)
	if m.all {
		states = nil
	}
	return func() tea.Msg {
		var rs []api.Request
		err := b.Call(ctx, api.Input{Operation: "list", States: states, Session: session}, &rs)
		return refreshed{rs, err, gen}
	}
}
func (m *reviewModel) selected() *api.Request {
	if i, ok := m.queue.SelectedItem().(reviewItem); ok {
		r := i.request
		return &r
	}
	return nil
}
func (m *reviewModel) layout() {
	// Header, subtitle, status and a two-line footer surround the panes.
	w, h := max(m.width-6, 20), max(m.height-5, 5)
	if m.width >= 100 {
		left := max(min(48, w/2), w*2/5)
		m.queue.SetSize(left, h)
		m.details.SetWidth(max(w-left-3, 15))
		m.details.SetHeight(h)
	} else {
		top := max(6, h/2)
		m.queue.SetSize(w, top)
		m.details.SetWidth(w)
		m.details.SetHeight(max(4, h-top))
	}
	m.reason.SetWidth(max(m.width-18, 10))
}
func (m *reviewModel) syncDetails() {
	r := m.selected()
	if m.confirm != nil {
		r = m.confirm
	}
	if r == nil {
		// Keep selectedID: a filtered refresh briefly shows no items.
		m.details.SetContent("No request selected. New requests appear automatically.\n\n/ filter · h include history · r refresh")
		return
	}
	if m.selectedID != r.ID {
		m.details.GotoTop()
		m.selectedID = r.ID
	}
	m.details.SetContent(wrap(requestText(*r), m.details.Width()))
}
func (m *reviewModel) setError(err error) { m.status = err.Error(); m.errorStatus = true }

func (m *reviewModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	defer m.syncDetails()
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil
	case refreshTick:
		if !m.loading && !m.busy && m.confirm == nil && !m.queue.SettingFilter() {
			return m, tea.Batch(m.fetch(), tickReview())
		}
		return m, tickReview()
	case refreshed:
		if msg.generation != m.generation {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		items := make([]list.Item, len(msg.requests))
		for i, r := range msg.requests {
			items[i] = reviewItem{r}
		}
		row := m.queue.Index()
		cmd := m.queue.SetItems(items)
		m.selectID(m.selectedID, row)
		return m, cmd
	case decided:
		m.busy = false
		if msg.err != nil {
			m.setError(msg.err)
		} else {
			m.status = msg.request.ID + " " + msg.request.State
			m.errorStatus = false
		}
		return m, m.fetch()
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
		}
		m.status, m.errorStatus = "", false
		// ↑/↓ always move in the queue; PgUp/PgDn always scroll the details.
		if key == "pgup" || key == "pgdown" {
			var cmd tea.Cmd
			m.details, cmd = m.details.Update(msg)
			return m, cmd
		}
		if m.confirm != nil {
			switch key {
			case "esc":
				m.confirm = nil
				m.action = ""
				m.reason.Blur()
				return m, nil
			case "enter":
				r := *m.confirm
				action, reason := m.action, m.reason.Value()
				b, ctx := m.backend, m.ctx
				m.confirm = nil
				m.busy = true
				m.reason.Blur()
				return m, func() tea.Msg {
					var out api.Request
					err := b.Call(ctx, api.Input{Operation: "decide", ID: r.ID, Action: action, Reason: reason, ExpectDigest: r.Digest, Session: os.Getenv("AGSUDO_SESSION")}, &out)
					return decided{out, err}
				}
			}
			var cmd tea.Cmd
			m.reason, cmd = m.reason.Update(msg)
			return m, cmd
		}
		if m.queue.SettingFilter() {
			return m, m.updateQueue(msg)
		}
		switch key {
		case "q":
			return m, tea.Quit
		case "r":
			if !m.loading {
				return m, m.fetch()
			}
			return m, nil
		case "h":
			m.all = !m.all
			m.queue.ResetFilter()
			return m, m.fetch()
		case "a", "d":
			r := m.selected()
			if r == nil {
				return m, nil
			}
			if !r.Waiting() {
				m.setError(fmt.Errorf("%s is %s; only pending and needs_human requests can be decided", r.ID, r.State))
				return m, nil
			}
			m.details.GotoTop() // Show the whole command while confirming.
			m.confirm = r
			m.action = map[string]string{"a": "approve", "d": "deny"}[key]
			m.reason.SetValue("")
			return m, m.reason.Focus()
		}
	}
	return m, m.updateQueue(message)
}

// updateQueue passes a message to the list. Refreshes, filter results and
// clearing a filter keep the selected request; other keys move the selection.
func (m *reviewModel) updateQueue(msg tea.Msg) tea.Cmd {
	_, key := msg.(tea.KeyPressMsg)
	filtered, id := m.queue.FilterState() != list.Unfiltered, m.selectedID
	var cmd tea.Cmd
	m.queue, cmd = m.queue.Update(msg)
	if !key || filtered && m.queue.FilterState() == list.Unfiltered {
		m.selectID(id, m.queue.Index())
	}
	return cmd
}

// selectID keeps the cursor on request id when it is visible; otherwise it
// stays on the same row, so the next request moves up after a decision.
func (m *reviewModel) selectID(id string, row int) {
	visible := m.queue.VisibleItems()
	for i, item := range visible {
		if item.(reviewItem).request.ID == id {
			row = i
		}
	}
	m.queue.Select(max(min(row, len(visible)-1), 0))
}
func (m *reviewModel) View() tea.View {
	if m.width < 50 || m.height < 18 {
		v := tea.NewView("AG-sudo review\nResize to at least 50 columns × 18 rows.\nq quit")
		v.AltScreen = true
		return v
	}
	accent := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	header := accent.Render("AG-sudo") + "  Approval workbench"
	if m.session != "" {
		header += " · " + api.Safe(m.session, false)
	}
	mode := "pending and needs_human"
	if m.all {
		mode = "all requests, including history"
	}
	sub := muted.Render(mode + " · refreshes automatically")
	var content string
	if m.width >= 100 {
		left := lipgloss.NewStyle().Width(m.queue.Width() + 1).Render(m.queue.View())
		content = lipgloss.JoinHorizontal(lipgloss.Top, left, " │ ", m.details.View())
	} else {
		content = m.queue.View() + "\n" + m.details.View()
	}
	foot := "a approve  d deny  / filter  h history  q quit\n↑/↓ select  PgUp/PgDn details  r refresh"
	if m.confirm != nil {
		title := "DENY " + command(*m.confirm)
		if m.action == "approve" {
			title = "APPROVE " + commandLabel(*m.confirm)
		}
		foot = accent.Render(elide(title, m.width-4)) + "\n" + m.reason.View()
	}
	if m.busy {
		foot = "Saving decision…"
	}
	status := api.Safe(m.status, false)
	// Say when details continue below; only errors come first.
	if more := m.details.TotalLineCount() - m.details.YOffset() - m.details.Height(); more > 0 {
		hint := fmt.Sprintf("↓ %d more lines of details (PgDn)", more)
		if status == "" {
			status = hint
		} else if m.errorStatus {
			status += " · " + hint
		} else {
			status = hint + " · " + status
		}
	}
	style := muted
	if m.errorStatus {
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	}
	text := header + "\n" + sub + "\n" + content + "\n" + style.Render(ansi.Truncate(status, m.width-4, "…")) + "\n" + foot
	v := tea.NewView(lipgloss.NewStyle().Padding(0, 1).Width(m.width - 2).Render(text))
	v.AltScreen = true
	return v
}
