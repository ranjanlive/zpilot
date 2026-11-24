package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ranjanlive/zpilot/zengine/internal/control"
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// API is the part of the control client the UI uses; tests substitute a fake.
type API interface {
	State(ctx context.Context) (control.StateResponse, error)
	Events(ctx context.Context, since int64) ([]session.Event, error)
	Prompt(ctx context.Context, text string, attachments ...string) error
	Note(ctx context.Context, text string) error
	Command(ctx context.Context, line string) (engine.CommandResult, error)
	Stop(ctx context.Context) error
	Resume(ctx context.Context) error
	Stream(ctx context.Context, since int64, onEvent func(session.Event), onLive func(engine.Live)) error
}

// messages delivered to Update
type (
	loadedMsg struct {
		state  control.StateResponse
		events []session.Event
	}
	eventMsg session.Event
	liveMsg  engine.Live
	stateMsg control.StateResponse
	replyMsg struct {
		text, warn string
		res        engine.CommandResult
	}
	streamEnd struct{ err error }
	errMsg    struct{ err error }
)

// Model is the Bubble Tea model.
type Model struct {
	api    API
	ctx    context.Context
	cancel context.CancelFunc
	send   func(tea.Msg) // delivers messages from the stream goroutine

	vp  viewport.Model
	ta  textarea.Model
	sp  spinner.Model
	w   int
	h   int
	rdy bool

	st       control.StateResponse
	counters engine.Counters
	activity string
	blocks   []block
	tools    map[string]*toolBlock
	seq      int64
	stream   string // text streamed live and not yet in the log
	attach   []string
	banner   string
	stuck    bool // viewport follows the newest content
	quitting bool
	loadErr  error
}

// New creates the model. send is set by Run once the program exists.
func New(api API) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	ta := textarea.New()
	ta.Placeholder = "Message the agent…  (/help for commands)"
	ta.Prompt = "│ "
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.Focus()
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	return &Model{api: api, ctx: ctx, cancel: cancel, ta: ta, sp: sp, tools: map[string]*toolBlock{}, stuck: true}
}

// busy reports whether the agent is working (so input becomes a note).
func (m *Model) busy() bool {
	s := m.st.State
	return s == engine.StateWorking || s == engine.StateCompacting || s == engine.StatePaused
}

// Init loads the state and history, then the stream takes over.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.sp.Tick, m.load())
}

func (m *Model) load() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		defer cancel()
		st, err := m.api.State(ctx)
		if err != nil {
			return errMsg{err}
		}
		evs, err := m.api.Events(ctx, 0)
		if err != nil {
			return errMsg{err}
		}
		return loadedMsg{state: st, events: evs}
	}
}

// startStream feeds log and live events into the program until the context ends.
func (m *Model) startStream(since int64) {
	go func() {
		err := m.api.Stream(m.ctx, since,
			func(e session.Event) { m.send(eventMsg(e)) },
			func(l engine.Live) { m.send(liveMsg(l)) })
		m.send(streamEnd{err})
	}()
}

func (m *Model) layout() {
	if m.w == 0 {
		return
	}
	inputH := m.ta.Height() + 1
	head, foot := 3, inputH+2
	m.vp.Width = m.w
	m.vp.Height = max(3, m.h-head-foot)
	m.ta.SetWidth(m.w - 2)
	m.rebuild()
}

// rebuild re-renders every block into the viewport.
func (m *Model) rebuild() {
	if m.w == 0 {
		return
	}
	wasBottom := m.vp.AtBottom()
	var parts []string
	for _, b := range m.blocks {
		parts = append(parts, renderBlock(b, m.w-2))
	}
	if m.stream != "" {
		parts = append(parts, renderBlock(block{kind: "text", who: "agent", text: m.stream}, m.w-2))
	}
	m.vp.SetContent(strings.Join(parts, "\n\n"))
	if m.stuck || wasBottom {
		m.vp.GotoBottom()
	}
}

func (m *Model) add(bs ...block) {
	m.blocks = append(m.blocks, bs...)
}

// Update handles every message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h, m.rdy = msg.Width, msg.Height, true
		if m.vp.Width == 0 {
			m.vp = viewport.New(msg.Width, 5)
		}
		m.layout()

	case loadedMsg:
		m.st, m.counters = msg.state, msg.state.Counters
		for _, ev := range msg.events {
			m.applyEvent(ev)
		}
		m.rebuild()
		m.startStream(m.seq)

	case eventMsg:
		m.applyEvent(session.Event(msg))
		m.rebuild()

	case liveMsg:
		m.applyLive(engine.Live(msg))

	case stateMsg:
		m.st = control.StateResponse(msg)
		m.counters = m.st.Counters

	case replyMsg:
		if msg.warn != "" {
			m.add(block{kind: "error", text: msg.warn})
		}
		if msg.text != "" {
			m.add(block{kind: "system", text: msg.text})
		}
		if msg.res.Exit || msg.res.Shutdown || msg.res.NewSession {
			m.quitting = true
			m.cancel()
			if msg.res.NewSession {
				m.banner = "new"
			}
			return m, tea.Quit
		}
		m.rebuild()

	case streamEnd:
		if m.quitting || m.ctx.Err() != nil {
			return m, nil
		}
		m.st.State = engine.StateClosed
		m.banner = "The engine stopped or the connection was lost. The session is saved; reopen it with: zagent --session " + m.st.Session + " --ui cli"
		m.add(block{kind: "warn", text: m.banner})
		m.rebuild()

	case errMsg:
		m.loadErr = msg.err
		m.quitting = true
		return m, tea.Quit

	case spinner.TickMsg:
		var c tea.Cmd
		m.sp, c = m.sp.Update(msg)
		cmds = append(cmds, c)
		if m.busy() {
			cmds = append(cmds, m.refreshState())
		}

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		var c tea.Cmd
		m.vp, c = m.vp.Update(msg)
		m.stuck = m.vp.AtBottom()
		cmds = append(cmds, c)
	}
	return m, tea.Batch(cmds...)
}

// refreshState polls state occasionally so counters stay current even between events.
func (m *Model) refreshState() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Second)
		defer cancel()
		if st, err := m.api.State(ctx); err == nil {
			return stateMsg(st)
		}
		return nil
	}
}

func (m *Model) applyEvent(ev session.Event) {
	if ev.Seq != 0 {
		if ev.Seq <= m.seq {
			return
		}
		m.seq = ev.Seq
	}
	if ev.Kind == session.KindMessage && ev.Message != nil && ev.Message.Role == "assistant" {
		m.stream = "" // the durable message replaces the streamed preview
	}
	if ev.Kind == session.KindUsage && ev.Usage != nil {
		m.counters.Context = ev.Usage.PromptTokens
	}
	m.add(m.blocksForEvent(ev)...)
}

func (m *Model) applyLive(l engine.Live) {
	switch l.Kind {
	case engine.LiveText:
		m.stream += l.Text
		m.activity = "writing"
		m.rebuild()
	case engine.LiveThought:
		m.activity = "thinking"
	case engine.LiveToolCall:
		m.activity = "preparing " + l.Name
	case engine.LiveToolRun:
		m.activity = "running " + l.Name
	case engine.LiveToolDone:
		m.activity = "thinking"
	case engine.LiveState:
		m.st.State = l.State
		m.st.Detail = l.Detail
		if l.State == engine.StateIdle {
			m.activity = ""
		}
	case engine.LiveUsage:
		m.counters = l.Tokens
	case engine.LiveRetry:
		if l.Text == "reset_stream" {
			m.stream = ""
			m.rebuild()
		}
		if l.Detail != "" {
			m.banner = l.Detail
		}
	}
}

func (m *Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		if m.busy() {
			return m, m.doStop()
		}
		return m.detach()
	case "ctrl+d":
		return m.detach()
	case "esc":
		if m.busy() {
			return m, m.doStop()
		}
	case "pgup", "pgdown", "ctrl+u", "ctrl+b", "ctrl+f":
		var c tea.Cmd
		m.vp, c = m.vp.Update(k)
		m.stuck = m.vp.AtBottom()
		return m, c
	case "enter":
		return m, m.submit()
	}
	var c tea.Cmd
	m.ta, c = m.ta.Update(k)
	// grow the input up to six lines as the text wraps
	want := min(6, max(1, strings.Count(m.ta.Value(), "\n")+1))
	if want != m.ta.Height() {
		m.ta.SetHeight(want)
		m.layout()
	}
	return m, c
}

func (m *Model) detach() (tea.Model, tea.Cmd) {
	m.quitting = true
	m.cancel()
	return m, tea.Quit
}

func (m *Model) doStop() tea.Cmd {
	m.banner = "stopping…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		if err := m.api.Stop(ctx); err != nil {
			return replyMsg{warn: "could not stop: " + err.Error()}
		}
		return nil
	}
}

// submit routes the input: commands run on the engine, a busy agent gets a
// note, an idle one gets a prompt.
func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" && len(m.attach) == 0 {
		return nil
	}
	m.ta.Reset()
	m.ta.SetHeight(1)
	m.layout()
	m.stuck = true

	if strings.HasPrefix(text, "/attach ") {
		p, err := filepath.Abs(strings.TrimSpace(strings.TrimPrefix(text, "/attach ")))
		if err == nil {
			_, err = os.Stat(p)
		}
		if err != nil {
			m.add(block{kind: "error", text: "cannot attach: " + err.Error()})
		} else {
			m.attach = append(m.attach, p)
			m.add(block{kind: "note", text: "attached " + p + " — it goes with your next message"})
		}
		m.rebuild()
		return nil
	}
	busy := m.busy()
	attach := m.attach
	if !strings.HasPrefix(text, "/") && !busy {
		m.attach = nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		switch {
		case strings.HasPrefix(text, "/"):
			res, err := m.api.Command(ctx, text)
			if err != nil {
				return replyMsg{warn: err.Error()}
			}
			if !res.Handled { // a path such as /usr/bin: send it as a normal prompt
				if err := m.api.Prompt(ctx, text); err != nil {
					return replyMsg{warn: err.Error()}
				}
				return nil
			}
			return replyMsg{text: res.Reply, res: res}
		case busy:
			if err := m.api.Note(ctx, text); err != nil {
				return replyMsg{warn: err.Error()}
			}
			return replyMsg{text: ""}
		default:
			if err := m.api.Prompt(ctx, text, attach...); err != nil {
				return replyMsg{warn: err.Error()}
			}
			return nil
		}
	}
}

// ---- view ----

func (m *Model) View() string {
	if !m.rdy {
		return "connecting…"
	}
	return strings.Join([]string{m.header(), m.vp.View(), m.statusLine(), m.ta.View(), m.footer()}, "\n")
}

func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

func (m *Model) header() string {
	title := m.st.Title
	if title == "" {
		title = m.st.Session
	}
	left := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")).Render(" zagent ") + " " + oneLine(title, max(10, m.w/2-10))
	ctx := fmt.Sprintf("ctx %s", fmtTokens(m.counters.Context))
	if m.st.CompactAt > 0 {
		pct := float64(m.counters.Context) / float64(m.st.CompactAt)
		ctx += fmt.Sprintf("/%s %s", fmtTokens(m.st.CompactAt), bar(pct, 10))
	}
	est := ""
	if m.counters.Estimated {
		est = "~"
	}
	totals := fmt.Sprintf("↑%s%s ↓%s%s", est, fmtTokens(m.counters.Prompt), est, fmtTokens(m.counters.Completion+m.counters.Thought))
	right := dimStyle.Render(ctx + "  " + totals + "  " + m.st.Model)
	gap := max(1, m.w-lipgloss.Width(left)-lipgloss.Width(right))
	sub := dimStyle.Render(oneLine(m.st.Workspace, m.w-2))
	line := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(strings.Repeat("─", m.w))
	return left + strings.Repeat(" ", gap) + right + "\n" + sub + "\n" + line
}

func bar(p float64, w int) string {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	n := int(p*float64(w) + 0.5)
	return "▕" + strings.Repeat("█", n) + strings.Repeat("░", w-n) + "▏"
}

func (m *Model) statusLine() string {
	var s string
	switch m.st.State {
	case engine.StateWorking:
		act := m.activity
		if act == "" {
			act = "working"
		}
		s = m.sp.View() + " " + act + "…"
	case engine.StateCompacting:
		s = m.sp.View() + " compacting context…"
	case engine.StatePaused:
		s = warnStyle.Render("⏸ paused — network trouble; /resume retries now. ") + dimStyle.Render(oneLine(m.st.Detail, m.w-50))
	case engine.StateWaitingBG:
		s = okStyle.Render("● waiting for background tasks")
	case engine.StateClosed:
		s = errStyle.Render("● engine stopped")
	default:
		s = dimStyle.Render("● idle")
	}
	if m.banner != "" && m.banner != "new" {
		s += "  " + dimStyle.Render(oneLine(m.banner, m.w-lipgloss.Width(s)-4))
	}
	if n := len(m.attach); n > 0 {
		s += "  " + dimStyle.Render(fmt.Sprintf("📎 %d attached", n))
	}
	return s
}

func (m *Model) footer() string {
	if m.busy() {
		return dimStyle.Render("Enter sends a note (added to the next request) · Esc/Ctrl-C stops the agent · /help")
	}
	return dimStyle.Render("Enter sends · Alt-Enter newline · /attach FILE · /exit detaches (agent keeps running) · Ctrl-C detaches")
}

// ---- running ----

// Run opens the interface on the engine behind api and returns when the user
// leaves. It reports whether the user asked for a new session.
func Run(api API) (newSession bool, err error) {
	m := New(api)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	m.send = p.Send
	if _, err := p.Run(); err != nil {
		return false, err
	}
	m.cancel()
	if m.loadErr != nil {
		if errors.Is(m.loadErr, control.ErrNotRunning) {
			return false, fmt.Errorf("the engine is not running: %w", m.loadErr)
		}
		return false, m.loadErr
	}
	return m.banner == "new", nil
}
