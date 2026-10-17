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
	"golang.org/x/term"
)

type reviewItem struct{ request api.Request }

func (i reviewItem) Title() string { return i.request.State + "  " + command(i.request) }
func (i reviewItem) Description() string {
	return safe(i.request.Spec.Target.Name+" · "+i.request.Spec.Reason+" · "+i.request.ID, false)
}
func (i reviewItem) FilterValue() string {
	return command(i.request) + " " + i.request.Spec.Reason + " " + i.request.Spec.Session + " " + i.request.ID
}

type reviewBackend interface {
	Call(context.Context, api.Input, any) error
	ListAll(context.Context, []string, string) ([]api.Request, error)
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
type traced struct {
	id   string
	page api.Page
	err  error
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
	focusDetails       bool
	all, loading, busy bool
	generation         int
	selectedID         string
	confirm            *api.Request
	action, status     string
	errorStatus        bool
	traceText          string
}

func runTUI(ctx context.Context, c client.Client, states []string, session string, in io.Reader, out io.Writer) error {
	i, iOK := in.(*os.File)
	o, oOK := out.(*os.File)
	if !iOK || !oOK || !term.IsTerminal(int(i.Fd())) || !term.IsTerminal(int(o.Fd())) {
		return api.Err("terminal_required", "review --tui requires an interactive terminal; use review --json for Agents")
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
	v := viewport.New(viewport.WithWidth(60), viewport.WithHeight(18))
	v.SoftWrap = true
	input := textinput.New()
	input.Prompt = "Reason > "
	input.Placeholder = "Optional explanation; Enter confirms"
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
	return func() tea.Msg { rs, err := b.ListAll(ctx, states, session); return refreshed{rs, err, gen} }
}
func (m *reviewModel) selected() *api.Request {
	if i, ok := m.queue.SelectedItem().(reviewItem); ok {
		r := i.request
		return &r
	}
	return nil
}
func (m *reviewModel) layout() {
	w, h := max(m.width-6, 20), max(m.height-8, 5)
	if m.width >= 100 {
		left := min(48, w/2)
		m.queue.SetSize(left, h)
		m.details.SetWidth(max(w-left-3, 15))
		m.details.SetHeight(h)
	} else {
		top := max(6, h/3)
		m.queue.SetSize(w, top)
		m.details.SetWidth(w)
		m.details.SetHeight(max(4, h-top-1))
	}
	m.reason.SetWidth(max(m.width-18, 10))
}
func (m *reviewModel) syncDetails() {
	r := m.selected()
	if m.confirm != nil {
		r = m.confirm
	}
	if r == nil {
		m.details.SetContent("No request selected. New requests appear automatically.\n\n/ filter · h include history · r refresh")
		m.selectedID = ""
		return
	}
	if m.selectedID != r.ID {
		m.details.GotoTop()
		m.traceText = ""
		m.selectedID = r.ID
	}
	m.details.SetContent(requestText(*r) + m.traceText)
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
		oldID := ""
		if r := m.selected(); r != nil {
			oldID = r.ID
		}
		items := make([]list.Item, len(msg.requests))
		selected := 0
		for i, r := range msg.requests {
			items[i] = reviewItem{r}
			if r.ID == oldID {
				selected = i
			}
		}
		cmd := m.queue.SetItems(items)
		if !m.queue.IsFiltered() {
			m.queue.Select(selected)
		}
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
	case traced:
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		if msg.id == m.selectedID {
			var b strings.Builder
			b.WriteString("\n\nRecorded events (first 200; export for full history):\n")
			for _, e := range msg.page.Events {
				fmt.Fprintf(&b, "%d  %s  %s  UID %d\n", e.Sequence, e.At.Local().Format("15:04:05"), e.Type, e.Actor.UID)
			}
			m.traceText = b.String()
		}
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
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
					err := b.Call(ctx, api.Input{Operation: "decide", ID: r.ID, Action: action, Actor: "human:tui", Reason: reason, ExpectDigest: r.Digest, Recommend: "unknown"}, &out)
					return decided{out, err}
				}
			case "pgup", "pgdown":
				var cmd tea.Cmd
				m.details, cmd = m.details.Update(msg)
				return m, cmd
			}
			var cmd tea.Cmd
			m.reason, cmd = m.reason.Update(msg)
			return m, cmd
		}
		if m.queue.SettingFilter() {
			var cmd tea.Cmd
			m.queue, cmd = m.queue.Update(msg)
			return m, cmd
		}
		switch key {
		case "q":
			return m, tea.Quit
		case "tab":
			m.focusDetails = !m.focusDetails
			return m, nil
		case "r":
			if !m.loading {
				return m, m.fetch()
			}
			return m, nil
		case "h":
			m.all = !m.all
			m.queue.ResetFilter()
			return m, m.fetch()
		case "a", "d", "s":
			r := m.selected()
			if r == nil {
				return m, nil
			}
			if !r.Waiting() {
				m.setError(fmt.Errorf("request is %s; only pending requests can be reviewed", r.State))
				return m, nil
			}
			m.confirm = r
			m.action = map[string]string{"a": "approve", "d": "deny", "s": "defer"}[key]
			m.reason.SetValue("")
			return m, m.reason.Focus()
		case "t":
			r := m.selected()
			if r == nil {
				return m, nil
			}
			id, b, ctx := r.ID, m.backend, m.ctx
			return m, func() tea.Msg {
				var p api.Page
				err := b.Call(ctx, api.Input{Operation: "events", ID: id, Limit: 200}, &p)
				return traced{id, p, err}
			}
		}
		if m.focusDetails {
			var cmd tea.Cmd
			m.details, cmd = m.details.Update(msg)
			return m, cmd
		}
	}
	var cmd tea.Cmd
	m.queue, cmd = m.queue.Update(message)
	return m, cmd
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
		header += " · " + safe(m.session, false)
	}
	mode := "pending + selected states"
	if m.all {
		mode = "all requests / history"
	}
	focus := "queue"
	if m.focusDetails {
		focus = "details"
	}
	sub := muted.Render(mode + " · focus: " + focus + " · refreshes automatically")
	var content string
	if m.width >= 100 {
		left := lipgloss.NewStyle().Width(m.queue.Width() + 1).Render(m.queue.View())
		content = lipgloss.JoinHorizontal(lipgloss.Top, left, " │ ", m.details.View())
	} else {
		content = m.queue.View() + "\n" + m.details.View()
	}
	foot := "a approve · d deny · s defer · / filter · Tab focus · t trace\nh history · r refresh · ↑/↓ navigate · PgUp/PgDn scroll · q quit"
	if m.confirm != nil {
		note := ""
		if m.action == "approve" && m.confirm.Spec.Scope == "shell_session" {
			note = " · authorizes the entire shell session"
		}
		if m.action == "defer" {
			note += " · moves to needs_human for human review"
		}
		foot = accent.Render(strings.ToUpper(m.action)+" "+m.confirm.ID+note) + "\n" + m.reason.View() + "\nEnter confirm · Esc back · PgUp/PgDn inspect"
	}
	if m.busy {
		foot = "Saving decision…"
	}
	status := safe(m.status, false)
	if m.loading && status == "" {
		status = "Refreshing…"
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
