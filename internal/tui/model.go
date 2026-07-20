package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fastclaw-ai/fastclaw/internal/cliclient"
)

// Client is the slice of cliclient.Client the TUI consumes; an
// interface so tests can fake the gateway.
type Client interface {
	Agents(ctx context.Context) ([]cliclient.Agent, error)
	Sessions(ctx context.Context, agentID string) ([]cliclient.Session, error)
	History(ctx context.Context, agentID, sessionID string) ([]cliclient.HistoryMessage, error)
	Stream(ctx context.Context, agentID, sessionID, message string, on func(cliclient.Event)) error
	Steer(ctx context.Context, agentID, sessionID, message string) (bool, error)
	RenameSession(ctx context.Context, sessionID, title string) error
	BaseURL() string
}

// Options configures a TUI run.
type Options struct {
	Client    Client
	Agent     cliclient.Agent
	Agents    []cliclient.Agent
	SessionID string
	// LoadHistory replays the session's archived turns on startup
	// (used by --resume/--continue).
	LoadHistory bool
	Version     string
}

// Tea messages.
type streamEvtMsg struct{ ev cliclient.Event }
type streamDoneMsg struct{ err error }
type historyMsg struct {
	msgs []cliclient.HistoryMessage
	err  error
}
type sessionPickerMsg struct {
	sessions []cliclient.Session
	err      error
}
type shellResultMsg struct {
	output string
	err    error
}
type steerResultMsg struct {
	text     string
	buffered bool
	err      error
}
type tickMsg time.Time

type pickerKind int

const (
	pickerNone pickerKind = iota
	pickerSessions
	pickerAgents
)

// Model is the Bubble Tea model for fastclaw chat.
type Model struct {
	opts    Options
	client  Client
	program *tea.Program

	agent     cliclient.Agent
	agents    []cliclient.Agent
	sessionID string
	// sessionTitle mirrors the picker/rename title for the status bar.
	sessionTitle string

	width  int
	height int
	ready  bool

	viewport     viewport.Model
	spin         spinner.Model
	input        *inputModel
	userScrolled bool

	blocks []displayBlock

	// In-flight turn state.
	querying        bool
	turnStart       time.Time
	turnPending     bool
	streamCancel    context.CancelFunc
	streamed        strings.Builder
	streamedContent bool
	toolsByID       map[string]*toolState
	subagentNote    string
	queued          []string

	// Overlay picker.
	picker     *pickerModel
	pickerType pickerKind

	// Slash autocomplete.
	slashMatches []slashCommand

	errMsg   string
	ctrlCArm time.Time
}

// NewModel builds the chat model. Call SetProgram before Run.
func NewModel(opts Options) *Model {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = lipgloss.NewStyle().Foreground(colPrimary)
	return &Model{
		opts:      opts,
		client:    opts.Client,
		agent:     opts.Agent,
		agents:    opts.Agents,
		sessionID: opts.SessionID,
		spin:      sp,
		input:     newInputModel(),
		toolsByID: make(map[string]*toolState),
	}
}

func (m *Model) SetProgram(p *tea.Program) { m.program = p }

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, m.tickCmd()}
	if m.opts.LoadHistory {
		cmds = append(cmds, m.loadHistoryCmd())
	}
	return tea.Batch(cmds...)
}

func (m *Model) tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) loadHistoryCmd() tea.Cmd {
	agentID, sessionID := m.agent.ID, m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		msgs, err := m.client.History(ctx, agentID, sessionID)
		return historyMsg{msgs: msgs, err: err}
	}
}

// ─── Update ─────────────────────────────────────────────

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		// Degenerate ptys (script/expect, some CI shells) report 0x0;
		// rendering assumes sane minimums.
		m.width, m.height = max(msg.Width, 20), max(msg.Height, 8)
		if !m.ready {
			m.ready = true
			m.viewport = viewport.New(m.width, m.contentHeight())
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = m.contentHeight()
		}
		m.input.SetWidth(m.width)
		m.refreshViewport()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if m.querying {
			m.refreshViewport()
		}
		return m, cmd

	case tickMsg:
		if m.querying {
			m.refreshViewport()
		}
		return m, m.tickCmd()

	case historyMsg:
		if msg.err != nil {
			m.errMsg = "load history: " + msg.err.Error()
		} else {
			m.blocks = m.blocks[:0]
			for _, h := range msg.msgs {
				switch h.Role {
				case "user":
					m.blocks = append(m.blocks, displayBlock{Kind: blockUser, Content: h.Content})
				case "assistant":
					m.blocks = append(m.blocks, displayBlock{Kind: blockAssistant, Content: h.Content})
				}
			}
			if len(msg.msgs) > 0 {
				m.appendSystem(fmt.Sprintf("Resumed session (%d messages)", len(msg.msgs)), false)
			}
		}
		m.refreshViewport()
		return m, nil

	case sessionPickerMsg:
		if msg.err != nil {
			m.errMsg = "load sessions: " + msg.err.Error()
			m.refreshViewport()
			return m, nil
		}
		items := make([]pickerItem, 0, len(msg.sessions))
		for _, s := range msg.sessions {
			title := strings.TrimSpace(s.Title)
			if title == "" {
				title = strings.TrimSpace(s.Preview)
			}
			if title == "" {
				title = s.ID
			}
			items = append(items, pickerItem{ID: s.ID, Title: truncateANSI(title, 48), Desc: formatRelativeTime(s.UpdatedAt)})
		}
		m.picker = newPicker("Switch session", items, m.width)
		m.pickerType = pickerSessions
		m.input.Blur()
		return m, nil

	case streamEvtMsg:
		m.handleStreamEvent(msg.ev)
		m.refreshViewport()
		return m, nil

	case streamDoneMsg:
		return m.finishTurn(msg.err)

	case steerResultMsg:
		if msg.err != nil {
			m.errMsg = "steer: " + msg.err.Error()
		} else if msg.buffered {
			m.appendSystem("↪ Steered into the current turn: "+msg.text, false)
		} else {
			// No in-flight turn on the server; send it as a normal
			// turn once the local stream settles.
			m.queued = append(m.queued, msg.text)
			m.appendSystem("Queued; will send when this turn finishes: "+msg.text, false)
		}
		m.refreshViewport()
		return m, nil

	case shellResultMsg:
		out := strings.TrimRight(msg.output, "\n")
		if msg.err != nil {
			if out != "" {
				out += "\n"
			}
			out += msg.err.Error()
		}
		if out == "" {
			out = "(no output)"
		}
		m.appendSystem(out, msg.err != nil)
		m.refreshViewport()
		return m, nil
	}

	return m, nil
}

// ─── Key handling ───────────────────────────────────────

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	debugLog("key=%q querying=%v", key, m.querying)

	// Overlay picker swallows everything while open.
	if m.picker != nil {
		chosen, done := m.picker.Handle(key)
		if done {
			kind := m.pickerType
			m.picker = nil
			m.pickerType = pickerNone
			m.input.Focus()
			if chosen != nil {
				return m.applyPickerChoice(kind, *chosen)
			}
		}
		return m, nil
	}

	switch key {
	case "ctrl+c":
		if m.querying {
			m.detachTurn()
			return m, nil
		}
		if time.Since(m.ctrlCArm) < 2*time.Second {
			return m, tea.Quit
		}
		m.ctrlCArm = time.Now()
		m.appendSystem("Press Ctrl+C again to quit", false)
		m.refreshViewport()
		return m, nil

	case "ctrl+d":
		if m.input.Value() == "" {
			return m, tea.Quit
		}

	case "ctrl+l":
		m.blocks = nil
		m.errMsg = ""
		m.refreshViewport()
		return m, nil

	case "esc":
		if m.querying {
			m.detachTurn()
			return m, nil
		}
		if len(m.slashMatches) > 0 {
			m.slashMatches = nil
			return m, nil
		}
		m.input.Reset()
		return m, nil

	case "pgup", "ctrl+b":
		m.viewport.HalfPageUp()
		m.userScrolled = true
		return m, nil

	case "pgdown", "ctrl+f":
		m.viewport.HalfPageDown()
		if m.viewport.AtBottom() {
			m.userScrolled = false
		}
		return m, nil

	case "tab":
		if len(m.slashMatches) > 0 {
			m.input.SetValue(m.slashMatches[0].Name + " ")
			m.slashMatches = nil
			return m, nil
		}
	}

	submitted, cmd := m.input.Update(msg)
	m.updateSlashMatches()
	if !submitted {
		return m, cmd
	}

	text := m.input.Value()
	m.input.Reset()
	m.slashMatches = nil
	if text == "" {
		return m, nil
	}

	// Local shell escape.
	if strings.HasPrefix(text, "!") {
		shellCmd := strings.TrimSpace(strings.TrimPrefix(text, "!"))
		if shellCmd == "" {
			return m, nil
		}
		m.appendSystem("$ "+shellCmd, false)
		m.refreshViewport()
		return m, func() tea.Msg {
			out, err := exec.Command("bash", "-lc", shellCmd).CombinedOutput()
			return shellResultMsg{output: string(out), err: err}
		}
	}

	if name, args := canonicalSlash(text); name != "" {
		return m.handleSlash(name, args)
	}
	if strings.HasPrefix(text, "/") {
		m.appendSystem("Unknown command "+text+"; type /help for available commands", true)
		m.refreshViewport()
		return m, nil
	}

	if m.querying {
		// Turn in flight: steer it (Claude Code-style follow-up).
		return m, m.steerCmd(text)
	}
	return m, m.sendTurn(text)
}

func (m *Model) updateSlashMatches() {
	val := m.input.Value()
	if strings.HasPrefix(val, "/") && !strings.Contains(val, " ") && !strings.Contains(val, "\n") {
		m.slashMatches = matchSlashCommands(val)
	} else {
		m.slashMatches = nil
	}
}

func (m *Model) handleSlash(name, args string) (tea.Model, tea.Cmd) {
	switch name {
	case "/help":
		m.appendSystem(helpText(), false)

	case "/new":
		m.sessionID = cliclient.NewSessionID()
		m.sessionTitle = ""
		m.blocks = nil
		m.appendSystem("Started a new session", false)

	case "/clear":
		m.blocks = nil

	case "/web":
		m.appendSystem("Web dashboard: "+m.client.BaseURL(), false)

	case "/rename":
		if args == "" {
			m.appendSystem("Usage: /rename <title>", true)
			break
		}
		sessionID := m.sessionID
		client := m.client
		m.sessionTitle = args
		m.appendSystem("Renamed session: "+args, false)
		m.refreshViewport()
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.RenameSession(ctx, sessionID, args); err != nil {
				return shellResultMsg{err: fmt.Errorf("rename: %w", err)}
			}
			return nil
		}

	case "/sessions":
		agentID := m.agent.ID
		client := m.client
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			sessions, err := client.Sessions(ctx, agentID)
			return sessionPickerMsg{sessions: sessions, err: err}
		}

	case "/agents":
		items := make([]pickerItem, 0, len(m.agents))
		for _, a := range m.agents {
			desc := a.Model
			if a.ID == m.agent.ID {
				desc += " (current)"
			}
			items = append(items, pickerItem{ID: a.ID, Title: a.Name, Desc: desc})
		}
		m.picker = newPicker("Switch agent", items, m.width)
		m.pickerType = pickerAgents
		m.input.Blur()

	case "/exit":
		return m, tea.Quit
	}
	m.refreshViewport()
	return m, nil
}

func (m *Model) applyPickerChoice(kind pickerKind, it pickerItem) (tea.Model, tea.Cmd) {
	switch kind {
	case pickerSessions:
		m.sessionID = it.ID
		m.sessionTitle = it.Title
		m.blocks = nil
		m.refreshViewport()
		return m, m.loadHistoryCmd()
	case pickerAgents:
		for _, a := range m.agents {
			if a.ID == it.ID {
				m.agent = a
				break
			}
		}
		m.sessionID = cliclient.NewSessionID()
		m.sessionTitle = ""
		m.blocks = nil
		m.appendSystem(fmt.Sprintf("Switched to %s; started a new session", m.agent.Name), false)
		m.refreshViewport()
	}
	return m, nil
}

// ─── Turn lifecycle ─────────────────────────────────────

func (m *Model) sendTurn(text string) tea.Cmd {
	debugLog("sendTurn %q session=%s", text, m.sessionID)
	m.querying = true
	m.turnStart = time.Now()
	m.turnPending = false
	m.errMsg = ""
	m.subagentNote = ""
	m.streamed.Reset()
	m.streamedContent = false
	m.toolsByID = make(map[string]*toolState)
	m.userScrolled = false
	m.blocks = append(m.blocks, displayBlock{Kind: blockUser, Content: text})
	m.refreshViewport()

	ctx, cancel := context.WithCancel(context.Background())
	m.streamCancel = cancel
	client, agentID, sessionID := m.client, m.agent.ID, m.sessionID
	program := m.program
	return func() tea.Msg {
		err := client.Stream(ctx, agentID, sessionID, text, func(ev cliclient.Event) {
			if program != nil {
				program.Send(streamEvtMsg{ev: ev})
			}
		})
		return streamDoneMsg{err: err}
	}
}

func (m *Model) steerCmd(text string) tea.Cmd {
	client, agentID, sessionID := m.client, m.agent.ID, m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		buffered, err := client.Steer(ctx, agentID, sessionID, text)
		return steerResultMsg{text: text, buffered: buffered, err: err}
	}
}

// detachTurn stops watching the in-flight turn. The gateway keeps the
// agent running on its detached context and persists the reply.
func (m *Model) detachTurn() {
	if m.streamCancel != nil {
		m.streamCancel()
	}
}

func (m *Model) handleStreamEvent(ev cliclient.Event) {
	switch ev.Type {
	case "content_delta":
		if delta := ev.Str("delta"); delta != "" {
			m.streamed.WriteString(delta)
			m.streamedContent = true
		}

	case "content":
		// Full text for the segment; authoritative when no deltas
		// streamed (some providers only emit the final content).
		if !m.streamedContent {
			m.streamed.Reset()
			m.streamed.WriteString(ev.Str("content"))
		}
		m.flushStreamedText()

	case "tool_call":
		m.flushStreamedText()
		ts := &toolState{ID: ev.Str("id"), Name: ev.Str("name"), Started: time.Now()}
		if ts.Name == "" {
			return
		}
		if ts.ID != "" {
			m.toolsByID[ts.ID] = ts
		}
		if n := len(m.blocks); n > 0 && m.blocks[n-1].Kind == blockTool {
			m.blocks[n-1].Tools = append(m.blocks[n-1].Tools, ts)
		} else {
			m.blocks = append(m.blocks, displayBlock{Kind: blockTool, Tools: []*toolState{ts}})
		}

	case "tool_result":
		id := ev.Str("id")
		ts := m.toolsByID[id]
		if ts == nil {
			// Result for a call we never saw (reconnect edge); show it
			// as a bare completion line.
			ts = &toolState{ID: id, Name: "tool", Started: time.Now()}
			if n := len(m.blocks); n > 0 && m.blocks[n-1].Kind == blockTool {
				m.blocks[n-1].Tools = append(m.blocks[n-1].Tools, ts)
			} else {
				m.blocks = append(m.blocks, displayBlock{Kind: blockTool, Tools: []*toolState{ts}})
			}
		}
		ts.Done = true
		ts.Summary = toolResultSummary(ev.Str("result"))
		if isErr, ok := ev.Data["isError"].(bool); ok {
			ts.IsError = isErr
		}

	case "steer":
		if text := ev.Str("message"); text != "" {
			m.appendSystem("↪ Steered: "+text, false)
		}

	case "turn_pending":
		m.turnPending = true

	case "subagent_progress":
		if text := ev.Str("text"); text != "" {
			m.subagentNote = text
		} else if name := ev.Str("name"); name != "" {
			m.subagentNote = name
		}
	}
}

// flushStreamedText moves accumulated streaming text into a permanent
// assistant block.
func (m *Model) flushStreamedText() {
	text := strings.TrimSpace(m.streamed.String())
	m.streamed.Reset()
	m.streamedContent = false
	if text != "" {
		m.blocks = append(m.blocks, displayBlock{Kind: blockAssistant, Content: text})
	}
}

func (m *Model) finishTurn(err error) (tea.Model, tea.Cmd) {
	m.flushStreamedText()
	m.querying = false
	m.turnPending = false
	m.subagentNote = ""
	if m.streamCancel != nil {
		m.streamCancel()
		m.streamCancel = nil
	}
	switch {
	case err == nil:
		m.appendSystem("✦ Done in "+formatDuration(time.Since(m.turnStart)), false)
	case errIsCancel(err):
		m.appendSystem("Detached from this turn; the server keeps running and saves the reply (see /sessions)", false)
	default:
		m.appendSystem(err.Error(), true)
	}
	m.toolsByID = make(map[string]*toolState)
	m.input.Focus()
	m.refreshViewport()

	if len(m.queued) > 0 && err == nil {
		next := strings.Join(m.queued, "\n\n")
		m.queued = nil
		return m, m.sendTurn(next)
	}
	return m, nil
}

func errIsCancel(err error) bool {
	return err != nil && strings.Contains(err.Error(), context.Canceled.Error())
}

func (m *Model) appendSystem(content string, isErr bool) {
	kind := blockSystem
	if isErr {
		kind = blockError
	}
	m.blocks = append(m.blocks, displayBlock{Kind: kind, Content: content})
}

// ─── View ───────────────────────────────────────────────

func (m *Model) contentHeight() int {
	// header(1) + activity/suggestions(variable, min 0) + input + status(1)
	h := m.height - 4 - m.input.Height()
	if h < 3 {
		h = 3
	}
	return h
}

func (m *Model) refreshViewport() {
	if !m.ready {
		return
	}
	m.viewport.Height = m.contentHeight()
	m.viewport.SetContent(m.renderTranscript())
	if !m.userScrolled {
		m.viewport.GotoBottom()
	}
}

func (m *Model) renderTranscript() string {
	if len(m.blocks) == 0 && !m.querying {
		return m.renderWelcome()
	}
	var b strings.Builder
	for i, blk := range m.blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		switch blk.Kind {
		case blockUser:
			b.WriteString(renderUserBlock(blk.Content, m.width))
		case blockAssistant:
			b.WriteString(renderAssistantBlock(blk.Content, m.width))
		case blockTool:
			b.WriteString(renderToolBlock(blk.Tools, m.spin.View()))
		case blockSystem:
			b.WriteString(renderSystemBlock(blk.Content, false))
		case blockError:
			b.WriteString(renderSystemBlock(blk.Content, true))
		}
	}
	// Live streaming tail.
	if m.querying {
		if text := m.streamed.String(); strings.TrimSpace(text) != "" {
			b.WriteString("\n")
			b.WriteString(renderAssistantBlock(text, m.width))
		}
	}
	return b.String()
}

func (m *Model) renderWelcome() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("  " + stylePrimary.Bold(true).Render("● FastClaw") + "\n\n")
	b.WriteString("  " + styleMuted.Render("agent: ") + m.agent.Name)
	if m.agent.Model != "" {
		b.WriteString(styleMuted.Render("  ·  model: ") + m.agent.Model)
	}
	b.WriteString("\n")
	b.WriteString("  " + styleMuted.Render("web:   ") + m.client.BaseURL() + "\n\n")
	b.WriteString("  " + styleDim.Render("Type a message to start; /help for commands and keys") + "\n")
	return b.String()
}

func (m *Model) renderHeader() string {
	left := " " + stylePrimary.Bold(true).Render("● FastClaw") +
		styleMuted.Render(" · "+m.agent.Name)
	if m.agent.Model != "" {
		left += styleDim.Render(" · " + m.agent.Model)
	}
	title := m.sessionTitle
	if title == "" {
		title = m.sessionID
	}
	right := styleDim.Render(truncateANSI(title, 32) + " ")
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) renderActivity() string {
	elapsed := formatDuration(time.Since(m.turnStart))
	label := "Thinking…"
	for id := range m.toolsByID {
		if t := m.toolsByID[id]; t != nil && !t.Done {
			label = "Running " + t.Name + "…"
			break
		}
	}
	if m.turnPending {
		label = "Waiting for the follow-up turn…"
	}
	line := fmt.Sprintf("  %s %s %s",
		m.spin.View(),
		stylePrimary.Render(label),
		styleMuted.Render("("+elapsed+" · Esc to detach)"))
	if m.subagentNote != "" {
		line += styleDim.Render("  " + truncateANSI(m.subagentNote, 48))
	}
	return line
}

func (m *Model) renderSlashSuggestions() string {
	maxShow := min(len(m.slashMatches), 6)
	var inner strings.Builder
	for i := 0; i < maxShow; i++ {
		c := m.slashMatches[i]
		inner.WriteString(styleAccent.Bold(true).Render(c.Name) + styleMuted.Render("  "+c.Description))
		if i < maxShow-1 {
			inner.WriteString("\n")
		}
	}
	return stylePickerBox.Render(inner.String()) + "\n"
}

func (m *Model) renderStatusBar() string {
	var parts []string
	if m.querying {
		parts = append(parts, styleSuccess.Render("● replying"))
	} else {
		parts = append(parts, styleMuted.Render("○ idle"))
	}
	if len(m.queued) > 0 {
		parts = append(parts, styleMuted.Render(fmt.Sprintf("%d queued", len(m.queued))))
	}
	parts = append(parts, styleDim.Render(m.client.BaseURL()))
	left := " " + strings.Join(parts, styleDim.Render(" │ "))

	right := ""
	if pct := m.viewport.ScrollPercent(); pct < 1.0 {
		right = styleDim.Render(fmt.Sprintf("%d%% ", int(pct*100)))
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) View() string {
	if !m.ready {
		return "\n  " + m.spin.View() + " Starting…\n"
	}
	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	if m.picker != nil {
		b.WriteString(m.picker.View())
		b.WriteString("\n")
	} else {
		if m.querying {
			b.WriteString(m.renderActivity())
			b.WriteString("\n")
		}
		if len(m.slashMatches) > 0 {
			b.WriteString(m.renderSlashSuggestions())
		}
		if m.errMsg != "" {
			b.WriteString(styleError.Render("  ✗ "+m.errMsg) + "\n")
		}
		b.WriteString(m.input.View())
		b.WriteString("\n")
	}
	b.WriteString(m.renderStatusBar())
	return b.String()
}
