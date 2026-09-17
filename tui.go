package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// Theme
//
// kanagawa-dragon, lifted from me/tmux/conf.d/20-status.conf so the dashboard
// and the status bar underneath it are the same colours.
// ---------------------------------------------------------------------------

const (
	colFg     = lipgloss.Color("#c5c9c5") // primary text
	colMuted  = lipgloss.Color("#a6a69c") // secondary text
	colDim    = lipgloss.Color("#7a8382") // labels, rules
	colFaint  = lipgloss.Color("#393836") // separators
	colAccent = lipgloss.Color("#c4746e") // selection, the prefix red
	colBlue   = lipgloss.Color("#8ba4b0") // paths
	colGreen  = lipgloss.Color("#87a987") // attached, live
	colYellow = lipgloss.Color("#c4b28a") // leased
	colAqua   = lipgloss.Color("#8ea4a2") // window names
)

var (
	styleTitle    = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styleHost     = lipgloss.NewStyle().Foreground(colDim)
	styleRule     = lipgloss.NewStyle().Foreground(colFaint)
	styleHeading  = lipgloss.NewStyle().Foreground(colDim).Bold(true)
	styleSelected = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styleRow      = lipgloss.NewStyle().Foreground(colFg)
	styleMuted    = lipgloss.NewStyle().Foreground(colMuted)
	styleDim      = lipgloss.NewStyle().Foreground(colDim)
	styleLabel    = lipgloss.NewStyle().Foreground(colDim)
	stylePath     = lipgloss.NewStyle().Foreground(colBlue)
	styleLive     = lipgloss.NewStyle().Foreground(colGreen)
	styleLeased   = lipgloss.NewStyle().Foreground(colYellow)
	styleWindow   = lipgloss.NewStyle().Foreground(colAqua)
	styleErr      = lipgloss.NewStyle().Foreground(colAccent)
	styleKey      = lipgloss.NewStyle().Foreground(colMuted)
	styleKeyHint  = lipgloss.NewStyle().Foreground(colDim)
)

// refreshInterval is how often the tmux state is re-read. A list-sessions plus
// a list-windows per session costs a few milliseconds, so this is cheap - but
// it does run forever while the dashboard sits in a detached session. Phase 5
// should gate it on whether a client is actually attached.
const refreshInterval = 2 * time.Second

// agentPollInterval is the idle redraw rate while an agent window has focus.
// Each poll is one capture, and the next is only scheduled when the previous
// returns, so a slow capture throttles itself instead of piling up.
const agentPollInterval = 120 * time.Millisecond

// echoDelay is how long we wait after forwarding a keystroke before capturing
// again. Typing does not wait for the idle poll: the agent draws the character
// itself, so without a capture of its own every letter appeared up to a full
// poll interval late, which is most of what made the live view feel laggy. The
// few milliseconds give the program in the pane time to actually draw.
const echoDelay = 12 * time.Millisecond

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type focusPane int

const (
	focusSessions focusPane = iota
	focusWorktrees
)

type model struct {
	tmux tmuxClient
	th   treehouseClient

	state State
	err   error // refresh failures; owned by the tick

	// notice is feedback from something you just did. It is kept apart from
	// err because the refresh tick clears err every couple of seconds, which
	// would wipe an action's message before it could be read.
	notice string

	// Worktrees belong to the repo of the selected session, not to the
	// machine, so they are refetched whenever the selection moves to a
	// session in a different directory.
	worktrees []Worktree
	wtDir     string
	wtErr     error

	// preview is what the selected session's agent window currently shows.
	// It refreshes on every tick, so the dashboard answers "is this one
	// waiting on me?" without attaching.
	preview       []string
	agentCursor   cursorPos
	previewTarget string
	previewWin    Window
	previewErr    error

	modal modalState

	// agentFocus routes every keystroke to the selected session's agent window
	// and polls it fast enough to feel live. You never leave mainstreet.
	agentFocus bool

	// keys forwards keystrokes to the focused agent. It owns a goroutine, so
	// the event loop never waits on a fork, and it is the only writer, so the
	// order you typed in is the order tmux sees.
	keys *keySender

	// capturing is the target of the capture in flight, empty when there is
	// none, and pollQueued says whether the next poll tick is already booked.
	// Together they hold the live view to exactly one loop.
	//
	// Without them it forked: the 2s refresh asks for a capture too, and every
	// capture scheduled a fresh tick, so a second loop appeared every 2s and
	// none ever died. A minute in the agent view meant thirty concurrent
	// capture-panes per interval, and typing crawled.
	capturing  string
	pollQueued bool

	// showWorktrees keeps the pool pane out of the way until asked for. While
	// hidden its treehouse call is skipped entirely, so moving the cursor
	// costs nothing.
	showWorktrees bool

	// self is the session mainstreet runs in, when it runs inside tmux.
	self string

	cursor   int
	wtCursor int
	focus    focusPane

	filtering bool
	filter    string

	width  int
	height int
}

type stateMsg struct {
	st  State
	err error
}

type worktreeMsg struct {
	dir string
	wts []Worktree
	err error
}

type previewMsg struct {
	target string
	lines  []string
	cursor cursorPos
	err    error
}

type agentPollMsg struct{}

// echoMsg asks for a capture shortly after a keystroke was forwarded.
type echoMsg struct{}

// keyErrMsg is a send-keys failure, surfaced from the sender's goroutine.
type keyErrMsg struct{ err error }

type tickMsg time.Time

func newModel() model {
	r := execRunner{}
	t := tmuxClient{r: r}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return model{
		tmux: t,
		th:   treehouseClient{r: r},
		self: t.CurrentSession(ctx),
		keys: newKeySender(t),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(refreshCmd(m.tmux), tickCmd(), keyErrCmd(m.keys))
}

func refreshCmd(t tmuxClient) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := gather(ctx, t)
		return stateMsg{st: st, err: err}
	}
}

func worktreeCmd(th treehouseClient, dir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		wts, err := th.Status(ctx, dir)
		return worktreeMsg{dir: dir, wts: wts, err: err}
	}
}

// previewCmd captures a pane, and its cursor too when the pane is live. The
// idle preview - which nobody types into - asks for the cheaper capture that
// leaves the cursor out.
func previewCmd(t tmuxClient, tgt string, wantCursor bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if !wantCursor {
			lines, err := t.CapturePaneANSI(ctx, tgt)
			return previewMsg{target: tgt, lines: lines, err: err}
		}
		lines, cur, err := t.CaptureAgent(ctx, tgt)
		return previewMsg{target: tgt, lines: lines, cursor: cur, err: err}
	}
}

func agentPollCmd() tea.Cmd {
	return tea.Tick(agentPollInterval, func(time.Time) tea.Msg { return agentPollMsg{} })
}

func echoCmd() tea.Cmd {
	return tea.Tick(echoDelay, func(time.Time) tea.Msg { return echoMsg{} })
}

// keyErrCmd parks on the sender's error channel. It re-arms itself, so one
// failed send-keys does not take the reporting with it.
func keyErrCmd(s *keySender) tea.Cmd {
	return func() tea.Msg { return keyErrMsg{err: <-s.errs} }
}

// capture asks for a new frame unless one is already on its way. Dropping the
// duplicate is the point: a keystroke, the poll tick and the refresh can all
// want a frame at the same moment, and concurrent capture-panes only race to
// draw the same pane.
func (m *model) capture() tea.Cmd {
	if m.previewTarget == "" || m.capturing == m.previewTarget {
		return nil
	}
	m.capturing = m.previewTarget
	return previewCmd(m.tmux, m.previewTarget, m.agentFocus)
}

// queuePoll books the next idle frame of the live view, at most one deep.
func (m *model) queuePoll() tea.Cmd {
	if m.pollQueued || !m.agentFocus {
		return nil
	}
	m.pollQueued = true
	return agentPollCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(refreshCmd(m.tmux), tickCmd())

	case stateMsg:
		m.err = msg.err
		if msg.err == nil {
			m.state = msg.st
			m.clampCursor()
		}
		cmd := m.syncSelection(true)
		return m, cmd

	case previewMsg:
		if msg.target == m.capturing {
			m.capturing = ""
		}
		if msg.target == m.previewTarget {
			m.preview, m.previewErr, m.agentCursor = msg.lines, msg.err, msg.cursor
		}
		// Queue the next frame even for a response we have moved away from,
		// or switching sessions while live would strand the loop.
		return m, m.queuePoll()

	case agentPollMsg:
		m.pollQueued = false
		if !m.agentFocus {
			return m, nil
		}
		return m, m.capture()

	case echoMsg:
		return m, m.capture()

	case keyErrMsg:
		m.notice = msg.err.Error()
		return m, keyErrCmd(m.keys)

	case actionMsg:
		if msg.err != nil {
			m.notice = msg.err.Error()
			return m, nil
		}
		return m, refreshCmd(m.tmux)

	case attachDoneMsg:
		if msg.err != nil {
			m.notice = msg.err.Error()
		}
		return m, refreshCmd(m.tmux)

	case worktreeMsg:
		// A response for a directory we have already moved away from is stale.
		if msg.dir != m.wtDir {
			return m, nil
		}
		m.worktrees, m.wtErr = msg.wts, msg.err
		if m.wtCursor >= len(m.worktrees) {
			m.wtCursor = 0
		}
		return m, nil

	case tea.KeyMsg:
		if m.modal.active() {
			return m.handleModalKey(msg)
		}
		if m.agentFocus {
			return m.handleAgentKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.notice = ""
	if m.filtering {
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEsc:
			m.filtering, m.filter = false, ""
			m.clampCursor()
			cmd := m.syncSelection(false)
			return m, cmd
		case tea.KeyEnter:
			m.filtering = false
			return m, nil
		case tea.KeyBackspace:
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
			}
			m.clampCursor()
			cmd := m.syncSelection(false)
			return m, cmd
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(msg.Runes)
			m.clampCursor()
			cmd := m.syncSelection(false)
			return m, cmd
		}
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "enter":
		return m.attachSelected()
	case "a":
		return m.focusAgent()
	case "A":
		return m.attachAgent()
	case "n":
		m.openNewSession()
		return m, nil
	case "d":
		if s, ok := m.selected(); ok {
			m.openConfirmKill(s)
		}
		return m, nil
	case "w":
		m.showWorktrees = !m.showWorktrees
		if !m.showWorktrees && m.focus == focusWorktrees {
			m.focus = focusSessions
		}
		if m.showWorktrees {
			m.wtDir = "" // force a fetch; it was skipped while hidden
			cmd := m.syncSelection(false)
			return m, cmd
		}
		return m, nil
	case "/":
		m.filtering = true
		return m, nil
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.clampCursor()
			cmd := m.syncSelection(false)
			return m, cmd
		}
		return m, nil
	case "R":
		return m, refreshCmd(m.tmux)
	case "tab":
		if !m.showWorktrees {
			return m, nil // nothing to move between
		}
		if m.focus == focusSessions {
			m.focus = focusWorktrees
		} else {
			m.focus = focusSessions
		}
		return m, nil
	case "j", "down":
		cmd := m.move(1)
		return m, cmd
	case "k", "up":
		cmd := m.move(-1)
		return m, cmd
	case "g", "home":
		cmd := m.moveTo(0)
		return m, cmd
	case "G", "end":
		cmd := m.moveTo(m.listLen() - 1)
		return m, cmd
	}
	return m, nil
}

func (m *model) move(delta int) tea.Cmd { return m.moveTo(m.activeCursor() + delta) }

func (m *model) moveTo(i int) tea.Cmd {
	n := m.listLen()
	if n == 0 {
		return nil
	}
	if i < 0 {
		i = 0
	}
	if i > n-1 {
		i = n - 1
	}
	if m.focus == focusWorktrees {
		m.wtCursor = i
		return nil
	}
	m.cursor = i
	return m.syncSelection(false)
}

func (m model) activeCursor() int {
	if m.focus == focusWorktrees {
		return m.wtCursor
	}
	return m.cursor
}

func (m model) listLen() int {
	if m.focus == focusWorktrees {
		return len(m.worktrees)
	}
	return len(m.visible())
}

func (m *model) clampCursor() {
	if n := len(m.visible()); m.cursor >= n {
		m.cursor = max(0, n-1)
	}
}

// syncSelection refetches whatever depends on which session is selected. The
// worktree pool only moves when the directory changes, so holding down j does
// not spawn a treehouse per keystroke; the agent preview additionally refreshes
// on every tick, which is what `force` is for.
func (m *model) syncSelection(force bool) tea.Cmd {
	s, ok := m.selected()
	if !ok {
		m.previewTarget, m.preview, m.previewWin = "", nil, Window{}
		return nil
	}
	var cmds []tea.Cmd

	if m.showWorktrees && s.Dir != m.wtDir {
		m.wtDir = s.Dir
		m.worktrees, m.wtErr, m.wtCursor = nil, nil, 0
		cmds = append(cmds, worktreeCmd(m.th, s.Dir))
	}

	if w, ok := agentWindow(s); ok {
		tgt := target(s.Name, w.Index)
		changed := tgt != m.previewTarget
		if changed {
			m.previewTarget, m.preview, m.previewErr = tgt, nil, nil
		}
		m.previewWin = w
		// While the agent is live its own loop owns the frame rate. Forcing a
		// capture on every refresh tick as well is what used to fork a second
		// loop each time the tick ran.
		if changed || (force && !m.agentFocus) {
			cmds = append(cmds, m.capture())
		}
	} else {
		m.previewTarget, m.preview, m.previewWin = "", nil, Window{}
	}
	return tea.Batch(cmds...)
}

// attachSelected hands the terminal to a session. Inside tmux a nested attach
// is impossible, so we move the client instead - but that means detaching
// leaves tmux rather than returning here. The detach-returns-to-mainstreet
// loop wants mainstreet running in its own terminal, not in a tmux pane.
func (m model) attachSelected() (tea.Model, tea.Cmd) {
	s, ok := m.selected()
	if !ok {
		return m, nil
	}
	return m.attachTo(s.Name, nil)
}

// focusAgent makes the agent window live inside the dashboard: keys are
// forwarded to it with send-keys and the pane is re-captured several times a
// second. tmux happily drives a window that is not its session's current one,
// so this disturbs nothing about the target session.
func (m model) focusAgent() (tea.Model, tea.Cmd) {
	s, ok := m.selected()
	if !ok {
		return m, nil
	}
	w, found := agentWindow(s)
	if !found {
		m.notice = fmt.Sprintf("%s has no agent window", s.Name)
		return m, nil
	}
	m.agentFocus = true
	m.previewWin = w
	m.previewTarget = target(s.Name, w.Index)
	return m, tea.Batch(m.capture(), m.queuePoll())
}

// handleAgentKey forwards everything to the agent, so the escape hatch has to
// be a key an agent will not want: ctrl+] , the old telnet convention.
func (m model) handleAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+]" {
		m.agentFocus = false
		m.notice = ""
		return m, nil
	}
	key, literal, ok := tmuxKey(msg)
	if !ok {
		return m, nil
	}
	// Handing the key to the sender is a channel write, so the next keystroke
	// is read while this one is still being spawned. Ordering comes from the
	// sender being the only writer; failures come back as keyErrMsg.
	m.keys.send(keystroke{target: m.previewTarget, key: key, literal: literal})
	return m, echoCmd()
}

// attachAgent lands you in the agent window rather than wherever the session
// was last left. This is the common case: the dashboard exists to get you to an
// agent, and `↵` alone would drop you in whatever window you happened to leave.
func (m model) attachAgent() (tea.Model, tea.Cmd) {
	s, ok := m.selected()
	if !ok {
		return m, nil
	}
	w, found := agentWindow(s)
	if !found {
		m.notice = fmt.Sprintf("%s has no agent window", s.Name)
		return m, nil
	}
	return m.attachTo(s.Name, &w)
}

// attachTo optionally selects a window before handing over the terminal.
// select-window is run synchronously - it is a few milliseconds, and it has to
// land before the attach or you arrive in the wrong window.
func (m model) attachTo(session string, win *Window) (tea.Model, tea.Cmd) {
	if win != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := m.tmux.SelectWindow(ctx, target(session, win.Index)); err != nil {
			m.notice = err.Error()
			return m, nil
		}
	}
	if insideTmux() {
		return m, switchClientCmd(m.tmux, session)
	}
	argv := attachArgv(session)
	return m, tea.ExecProcess(exec.Command(argv[0], argv[1:]...), func(err error) tea.Msg {
		return attachDoneMsg{err: err}
	})
}

// visible is the session list after the filter, which matches on name and
// directory - the directory matters because a session's name can lie about
// what it is working on.
func (m model) visible() []Session {
	if m.filter == "" {
		return m.state.Sessions
	}
	needle := strings.ToLower(m.filter)
	var out []Session
	for _, s := range m.state.Sessions {
		if strings.Contains(strings.ToLower(s.Name), needle) ||
			strings.Contains(strings.ToLower(s.Dir), needle) {
			out = append(out, s)
		}
	}
	return out
}

func (m model) selected() (Session, bool) {
	ss := m.visible()
	if len(ss) == 0 || m.cursor >= len(ss) {
		return Session{}, false
	}
	return ss[m.cursor], true
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	header := m.headerView()
	footer := m.footerView()
	bodyH := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyH < 3 {
		bodyH = 3
	}

	// Columns: leftW | divider(1) | gutter(1) + detail. The gutter comes from
	// PaddingLeft on the right pane so the arithmetic stays in one place.
	leftW := min(m.listWidth(), m.width-20)
	rightW := m.width - leftW - 1

	// The worktree pane takes a slice off the bottom of the left column, but
	// only when it has been asked for.
	left := m.sessionsView(leftW, bodyH)
	if m.showWorktrees {
		wtH := clamp(bodyH/3, 5, 12)
		left = lipgloss.JoinVertical(lipgloss.Left,
			m.sessionsView(leftW, bodyH-wtH),
			m.worktreesView(leftW, wtH),
		)
	}
	divider := styleRule.Render(strings.TrimRight(strings.Repeat("│\n", bodyH), "\n"))
	right := m.detailView(rightW-1, bodyH)

	if m.modal.active() {
		return lipgloss.JoinVertical(lipgloss.Left,
			header, m.modalView(m.width, bodyH), footer)
	}

	body := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(leftW).Render(left),
		divider,
		lipgloss.NewStyle().Width(rightW).PaddingLeft(1).Render(right),
	)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// listWidth sizes the session column to the longest name rather than a
// fraction of the terminal, so the agent pane gets everything left over.
func (m model) listWidth() int {
	longest := 0
	for _, s := range m.state.Sessions {
		if n := len([]rune(s.Name)); n > longest {
			longest = n
		}
	}
	return clamp(longest+6, 20, 34)
}

func (m model) headerView() string {
	title := styleTitle.Render(" mainstreet ")
	host := m.state.Host
	if host == "" {
		host = "…"
	}
	right := styleHost.Render(" " + host + " ")
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return title + styleRule.Render(strings.Repeat("─", gap)) + right
}

func (m model) footerView() string {
	if m.filtering {
		return styleRule.Render(strings.Repeat("─", m.width)) + "\n" +
			styleLabel.Render(" filter ") + styleRow.Render(m.filter+"▏")
	}
	keys := []struct{ k, d string }{
		{"↵", "attach"}, {"a", "live agent"}, {"n", "new"}, {"d", "kill"},
		{"w", "worktrees"}, {"/", "filter"}, {"R", "refresh"}, {"q", "quit"},
	}
	var b strings.Builder
	b.WriteString(" ")
	for _, k := range keys {
		b.WriteString(styleKey.Render(k.k) + " " + styleKeyHint.Render(k.d) + "  ")
	}
	if m.filter != "" {
		b.WriteString(styleLeased.Render("filter:" + m.filter))
	}
	rule := styleRule.Render(strings.Repeat("─", m.width))
	if m.agentFocus {
		return rule + "\n" +
			styleKey.Render(" ctrl+]") + styleKeyHint.Render(" back   ") +
			styleKeyHint.Render("keys go to ") + styleWindow.Render(m.previewTarget)
	}
	switch {
	case m.notice != "":
		return rule + "\n" + styleErr.Render(" "+fit(m.notice, m.width-1))
	case m.err != nil:
		return rule + "\n" + styleErr.Render(" "+fit(m.err.Error(), m.width-1))
	}
	return rule + "\n" + b.String()
}

func (m model) sessionsView(w, h int) string {
	ss := m.visible()
	heading := fmt.Sprintf("SESSIONS (%d)", len(ss))
	if m.filter != "" {
		heading = fmt.Sprintf("SESSIONS (%d/%d)", len(ss), len(m.state.Sessions))
	}
	lines := []string{
		styleHeading.Render(heading),
		styleRule.Render(strings.Repeat("─", w-1)),
	}

	rows := h - len(lines)
	start := scrollStart(m.cursor, rows, len(ss))
	for i := start; i < len(ss) && i-start < rows; i++ {
		lines = append(lines, m.sessionRow(ss[i], i == m.cursor && m.focus == focusSessions, w-1))
	}
	if len(ss) == 0 {
		lines = append(lines, styleDim.Render("  no sessions"))
	}
	return strings.Join(lines, "\n")
}

// sessionRow is deliberately just the name. Window count and age used to sit
// here, but they are already in the detail pane, and a narrow list leaves more
// width for the agent - which is what you are actually looking at.
func (m model) sessionRow(s Session, selected bool, w int) string {
	dot := "  "
	if s.Attached > 0 {
		dot = " " + styleLive.Render("●")
	}
	nameW := w - 2 - 2
	if nameW < 6 {
		nameW = 6
	}
	if selected {
		return styleSelected.Render("› ") + styleSelected.Render(fit(s.Name, nameW)) + dot
	}
	return "  " + styleRow.Render(fit(s.Name, nameW)) + dot
}

func (m model) worktreesView(w, h int) string {
	lines := []string{
		styleHeading.Render("WORKTREES"),
		styleRule.Render(strings.Repeat("─", w-1)),
	}
	rows := h - len(lines)

	switch {
	case m.wtErr != nil && m.wtErr == errNoPool:
		lines = append(lines, styleDim.Render("  not a repository"))
	case m.wtErr != nil:
		lines = append(lines, styleErr.Render("  "+fit(m.wtErr.Error(), w-3)))
	case len(m.worktrees) == 0:
		lines = append(lines, styleDim.Render("  empty pool"))
	default:
		start := scrollStart(m.wtCursor, rows, len(m.worktrees))
		for i := start; i < len(m.worktrees) && i-start < rows; i++ {
			lines = append(lines, m.worktreeRow(m.worktrees[i],
				i == m.wtCursor && m.focus == focusWorktrees, w-1))
		}
	}
	return strings.Join(lines, "\n")
}

func (m model) worktreeRow(wt Worktree, selected bool, w int) string {
	name := wt.Name
	if name == "" {
		name = baseName(wt.Path)
	}
	status := "free"
	style := styleDim
	if wt.Leased {
		status = "leased"
		if wt.Holder != "" {
			status = "→ " + wt.Holder
		}
		style = styleLeased
	}
	nameW := w - 2 - lipgloss.Width(status) - 1
	if nameW < 6 {
		nameW = 6
	}
	marker, rendered := "  ", styleRow.Render(fit(name, nameW))
	if selected {
		marker = styleSelected.Render("› ")
		rendered = styleSelected.Render(fit(name, nameW))
	}
	return marker + rendered + " " + style.Render(status)
}

func (m model) detailView(w, h int) string {
	s, ok := m.selected()
	if !ok {
		return styleDim.Render("nothing selected")
	}

	// Stats on the left, windows on the right, so the agent preview below gets
	// the full remaining height instead of competing with the window list.
	winW := min(36, w/2)
	statsW := w - winW

	stats := []string{
		field("dir", stylePath.Render(fit(s.Dir, statsW-9))),
		field("created", styleRow.Render(s.Created.Format("Jan 2 15:04"))),
		field("activity", styleRow.Render(since(s.Activity)+" ago")),
		field("clients", clientsLabel(s.Attached)),
	}
	wins := make([]string, 0, len(s.Windows))
	for _, win := range s.Windows {
		wins = append(wins, windowRow(win, winW))
	}

	top := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(statsW).Render(strings.Join(stats, "\n")),
		lipgloss.NewStyle().Width(winW).Render(strings.Join(wins, "\n")),
	)

	lines := []string{
		styleTitle.Render(s.Name),
		styleRule.Render(strings.Repeat("─", w)),
	}
	lines = append(lines, strings.Split(top, "\n")...)
	lines = append(lines, "")

	previewH := h - len(lines) - 2 // heading + rule
	lines = append(lines, m.agentView(w, previewH)...)
	return strings.Join(lines, "\n")
}

// withCursor marks the character at a visible column with reverse video, which
// is what a block cursor is. The column is counted in visible cells, so escape
// sequences in the line do not shift it; when the cursor sits past the end of
// the line - the usual case, just after a prompt - the line is padded out and
// the block drawn on the space.
func withCursor(line string, col int) string {
	const (
		reverse = "\x1b[7m"
		normal  = "\x1b[27m"
	)
	var b strings.Builder
	visible := 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			start := i
			for i < len(line) && line[i] != 'm' && line[i] != 'K' {
				i++
			}
			if i < len(line) {
				i++
			}
			b.WriteString(line[start:i])
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		if visible == col {
			b.WriteString(reverse)
			b.WriteRune(r)
			b.WriteString(normal)
		} else {
			b.WriteRune(r)
		}
		visible++
		i += size
	}
	if visible <= col {
		b.WriteString(strings.Repeat(" ", col-visible))
		b.WriteString(reverse + " " + normal)
	}
	return b.String()
}

// agentView shows the agent window's screen. When focused it is the same pane,
// live: keys are being forwarded to it and the cursor is drawn. It deliberately
// does not take over the screen - the session list and stats stay put, and if
// you want the agent full-size you attach to the session instead.
func (m model) agentView(w, h int) []string {
	if m.previewTarget == "" {
		return []string{styleHeading.Render("AGENT"), styleDim.Render("  no agent window")}
	}
	head := styleHeading.Render("AGENT") + "  " +
		styleWindow.Render(fmt.Sprintf("%d:%s", m.previewWin.Index, m.previewWin.Name))
	if m.agentFocus {
		head = styleLive.Render(" ● live ") + " " + styleWindow.Render(m.previewTarget)
	}
	out := []string{head, styleRule.Render(strings.Repeat("─", w))}
	if h <= 0 {
		return out
	}

	switch {
	case m.previewErr != nil:
		return append(out, styleErr.Render("  "+fit(m.previewErr.Error(), w-3)))
	case len(m.preview) == 0 && !m.agentFocus:
		return append(out, styleDim.Render("  (blank)"))
	}

	// Trailing blank rows are trimmed from the capture, but the cursor is
	// addressed against the full pane and usually sits just past the last line
	// of output. Pad back out so the row indices line up again.
	screen := m.preview
	if m.agentFocus && m.agentCursor.visible && m.agentCursor.y >= len(screen) {
		padded := make([]string, m.agentCursor.y+1)
		copy(padded, screen)
		screen = padded
	}

	offset := 0
	if len(screen) > h {
		offset = len(screen) - h
		screen = screen[offset:]
	}
	for i, line := range screen {
		if m.agentFocus && m.agentCursor.visible &&
			offset+i == m.agentCursor.y && m.agentCursor.x < w {
			line = withCursor(line, m.agentCursor.x)
		}
		out = append(out, ansiFit(line, w))
	}
	return out
}

func windowRow(w Window, width int) string {
	idx := styleDim.Render(fmt.Sprintf("%3d", w.Index))
	name := styleWindow.Render(fit(w.Name, 10))
	cmd := w.Command
	if cmd == "" {
		cmd = "—"
	}
	live := "  "
	if w.Active {
		live = " " + styleLive.Render("●")
	}
	return idx + " " + name + " " + styleMuted.Render(fit(cmd, max(4, width-20))) + live
}

func clientsLabel(n int) string {
	switch n {
	case 0:
		return styleDim.Render("none")
	case 1:
		return styleLive.Render("1 attached")
	default:
		return styleLive.Render(fmt.Sprintf("%d attached", n))
	}
}

func field(label, value string) string {
	return styleLabel.Render(fmt.Sprintf("%-9s", label)) + value
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// fit truncates with an ellipsis and pads to exactly w columns.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) > w {
		if w == 1 {
			return "…"
		}
		return string(r[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-len(r))
}

// ansiFit truncates to w *visible* columns, passing escape sequences through
// without counting them. fit() cannot be used on captured agent output: it
// counts escape bytes as runes and will cut a sequence in half, spilling raw
// control characters onto the screen.
func ansiFit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			start := i
			for i < len(s) && s[i] != 'm' && s[i] != 'K' {
				i++
			}
			if i < len(s) {
				i++
			}
			b.WriteString(s[start:i])
			continue
		}
		if visible >= w {
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		visible++
		i += size
	}
	if strings.ContainsRune(s, 0x1b) {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// scrollStart keeps the cursor inside a window of `rows` lines.
func scrollStart(cursor, rows, total int) int {
	if rows <= 0 || total <= rows {
		return 0
	}
	start := cursor - rows/2
	if start < 0 {
		start = 0
	}
	if start > total-rows {
		start = total - rows
	}
	return start
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

func runTUI() error {
	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
