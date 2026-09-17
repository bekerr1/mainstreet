package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// runner is the single seam between mainstreet and the outside world. Every
// subprocess goes through it, so tests substitute a fake returning canned
// output and never touch a real tmux server. dir == "" inherits the working
// directory; treehouse needs it set because its pool is scoped to a repo.
type runner interface {
	run(ctx context.Context, dir string, argv ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, dir string, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %s", argv[0], msg)
		}
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return stdout.Bytes(), nil
}

// sep is tmux's field delimiter in our -F strings.
//
// It is a real tab. Do not "improve" this to a control character like \x1f:
// tmux renders non-printable bytes in format output as octal escapes, so the
// separator arrives as the four characters \037 and every line parses as one
// field. A tab passes through untouched, and a tab in a window name is
// pathological enough to ignore.
const sep = "\t"

type tmuxClient struct{ r runner }

var (
	sessionFormat = strings.Join([]string{
		"#{session_name}", "#{session_windows}",
		"#{session_created}", "#{session_activity}", "#{session_attached}",
	}, sep)

	windowFormat = strings.Join([]string{
		"#{window_index}", "#{window_name}", "#{window_active}",
		"#{pane_current_command}", "#{pane_current_path}",
	}, sep)
)

// Sessions returns every session on the local tmux server, each with its
// windows populated. A server with no sessions is not an error.
func (t tmuxClient) Sessions(ctx context.Context) ([]Session, error) {
	out, err := t.r.run(ctx, "", "tmux", "list-sessions", "-F", sessionFormat)
	if err != nil {
		if noServer(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []Session
	for _, line := range splitLines(out) {
		s, err := parseSession(line)
		if err != nil {
			return nil, err
		}
		if s.Windows, err = t.Windows(ctx, s.Name); err != nil {
			return nil, err
		}
		s.Dir = activeDir(s.Windows)
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func (t tmuxClient) Windows(ctx context.Context, session string) ([]Window, error) {
	out, err := t.r.run(ctx, "", "tmux", "list-windows", "-t", session, "-F", windowFormat)
	if err != nil {
		return nil, err
	}
	var windows []Window
	for _, line := range splitLines(out) {
		w, err := parseWindow(line)
		if err != nil {
			return nil, err
		}
		windows = append(windows, w)
	}
	return windows, nil
}

func parseSession(line string) (Session, error) {
	f := strings.Split(line, sep)
	if len(f) != 5 {
		return Session{}, fmt.Errorf("tmux: expected 5 session fields, got %d in %q", len(f), line)
	}
	count, err := strconv.Atoi(f[1])
	if err != nil {
		return Session{}, fmt.Errorf("tmux: window count %q: %w", f[1], err)
	}
	created, err := unixField(f[2])
	if err != nil {
		return Session{}, err
	}
	activity, err := unixField(f[3])
	if err != nil {
		return Session{}, err
	}
	attached, err := strconv.Atoi(f[4])
	if err != nil {
		return Session{}, fmt.Errorf("tmux: attached count %q: %w", f[4], err)
	}
	return Session{
		Name:        f[0],
		WindowCount: count,
		Created:     created,
		Activity:    activity,
		Attached:    attached,
	}, nil
}

func parseWindow(line string) (Window, error) {
	f := strings.Split(line, sep)
	if len(f) != 5 {
		return Window{}, fmt.Errorf("tmux: expected 5 window fields, got %d in %q", len(f), line)
	}
	idx, err := strconv.Atoi(f[0])
	if err != nil {
		return Window{}, fmt.Errorf("tmux: window index %q: %w", f[0], err)
	}
	return Window{
		Index:   idx,
		Name:    f[1],
		Active:  f[2] == "1",
		Command: f[3],
		Path:    f[4],
	}, nil
}

func unixField(s string) (time.Time, error) {
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("tmux: timestamp %q: %w", s, err)
	}
	return time.Unix(secs, 0), nil
}

func splitLines(out []byte) []string {
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// noServer reports whether err is tmux's "nothing running" complaint, which is
// a normal empty state rather than a failure.
func noServer(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no server running") ||
		strings.Contains(s, "error connecting") ||
		strings.Contains(s, "no current session")
}

// activeDir is where a session currently is, taken from its active window.
//
// It deliberately does not use #{session_path}: that records where the session
// was created, and sessions get created from $HOME, so it reads /home/brendan
// for most of them. The active window's pane path is always meaningful.
//
// This is a display value. Do not use it to decide which treehouse worktree a
// session occupies - a single window can wander outside the repo (an active
// window sitting in ~/.nebius would defeat the match). Prefix-match every
// window path for that.
func activeDir(windows []Window) string {
	for _, w := range windows {
		if w.Active {
			return w.Path
		}
	}
	if len(windows) > 0 {
		return windows[0].Path
	}
	return ""
}

// ---------------------------------------------------------------------------
// Session layout
// ---------------------------------------------------------------------------

type layoutWindow struct {
	Index int
	Name  string
}

// defaultLayout is the shape every new session gets. The agent leads at index
// 0 because reaching it is usually why you came, and tmux selects the first
// window on attach.
var defaultLayout = []layoutWindow{
	{0, "agent"},
	{1, "base"},
	{2, "build"},
	{3, "src"},
}

// NewSession creates a detached session laid out per layout.
//
// tmux puts a new session's first window at base-index, which is 1 in the me
// config, so index 0 cannot be created directly - the first window is created
// wherever tmux puts it and then moved. This is why the plan calls window 0
// "created explicitly".
func (t tmuxClient) NewSession(ctx context.Context, name, dir string, layout []layoutWindow) error {
	if len(layout) == 0 {
		return errors.New("tmux: refusing to create a session with no windows")
	}
	first := layout[0]
	if _, err := t.r.run(ctx, "", "tmux", "new-session",
		"-d", "-s", name, "-n", first.Name, "-c", dir); err != nil {
		return err
	}
	placed, err := t.Windows(ctx, name)
	if err != nil {
		return err
	}
	if len(placed) > 0 && placed[0].Index != first.Index {
		if _, err := t.r.run(ctx, "", "tmux", "move-window",
			"-s", target(name, placed[0].Index), "-t", target(name, first.Index)); err != nil {
			return err
		}
	}
	for _, w := range layout[1:] {
		if _, err := t.r.run(ctx, "", "tmux", "new-window",
			"-d", "-t", target(name, w.Index), "-n", w.Name, "-c", dir); err != nil {
			return err
		}
	}
	_, err = t.r.run(ctx, "", "tmux", "select-window", "-t", target(name, first.Index))
	return err
}

func (t tmuxClient) KillSession(ctx context.Context, name string) error {
	_, err := t.r.run(ctx, "", "tmux", "kill-session", "-t", name)
	return err
}

// SwitchClient moves an already-attached client to another session. Used when
// mainstreet is running inside tmux, where a nested `attach` is not possible.
func (t tmuxClient) SwitchClient(ctx context.Context, name string) error {
	_, err := t.r.run(ctx, "", "tmux", "switch-client", "-t", name)
	return err
}

// CapturePane returns what is currently on screen in a window, trailing blank
// lines removed. This is the agent preview: it answers "is this one waiting on
// me?" without attaching.
func (t tmuxClient) CapturePane(ctx context.Context, tgt string) ([]string, error) {
	out, err := t.r.run(ctx, "", "tmux", "capture-pane", "-p", "-t", tgt)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, nil
}

func target(session string, index int) string {
	return fmt.Sprintf("%s:%d", session, index)
}

// attachArgv is the command that hands the terminal over to a session. It is
// only correct when mainstreet is NOT inside tmux; inside, use SwitchClient.
func attachArgv(name string) []string {
	return []string{"tmux", "attach", "-t", name}
}

// insideTmux reports whether we are running in a tmux pane, which decides
// between switch-client and a child-process attach.
func insideTmux() bool { return os.Getenv("TMUX") != "" }

// agentWindow picks the window most likely to hold a coding agent. New
// sessions name it "agent"; sessions predating that convention name it
// "claude"; failing both, we look at what is actually running.
func agentWindow(s Session) (Window, bool) {
	for _, want := range []string{"agent", "claude"} {
		for _, w := range s.Windows {
			if strings.EqualFold(w.Name, want) {
				return w, true
			}
		}
	}
	for _, w := range s.Windows {
		switch strings.ToLower(w.Command) {
		case "claude", "codex", "pi", "aider", "goose":
			return w, true
		}
	}
	return Window{}, false
}

// CurrentSession reports the session mainstreet is itself running in, or ""
// when it is not inside tmux. Used to warn before killing the session the
// dashboard lives in - which works, and takes the dashboard with it.
func (t tmuxClient) CurrentSession(ctx context.Context) string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	out, err := t.r.run(ctx, "", "tmux", "display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SelectWindow makes a window the session's active one. It works on a detached
// session, which is what lets `a` land you directly in the agent window: select
// first, then attach.
func (t tmuxClient) SelectWindow(ctx context.Context, tgt string) error {
	_, err := t.r.run(ctx, "", "tmux", "select-window", "-t", tgt)
	return err
}

// CapturePaneANSI is CapturePane with colour escapes left in, for the live
// agent view. The escapes mean these lines must be truncated with an
// ANSI-aware helper and rendered without further styling - they carry their
// own.
func (t tmuxClient) CapturePaneANSI(ctx context.Context, tgt string) ([]string, error) {
	out, err := t.r.run(ctx, "", "tmux", "capture-pane", "-e", "-p", "-t", tgt)
	if err != nil {
		return nil, err
	}
	return trimBlankTail(strings.Split(strings.TrimRight(string(out), "\n"), "\n")), nil
}

// SendKeys forwards a run of keystrokes to a window. tmux routes them to that
// window's active pane, and the window does not have to be the session's
// current one - which is what lets mainstreet drive an agent without attaching
// to anything.
func (t tmuxClient) SendKeys(ctx context.Context, r keyRun) error {
	argv := []string{"tmux", "send-keys", "-t", r.target}
	if r.literal {
		// -l sends the argument as-is: no key-name lookup and no escape
		// processing, so a run can be joined into one argument and a typed
		// backslash stays a backslash.
		argv = append(argv, "-l", strings.Join(r.keys, ""))
	} else {
		argv = append(argv, r.keys...)
	}
	_, err := t.r.run(ctx, "", argv...)
	return err
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' && s[i] != 'K' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// tmuxKeyNames maps Bubble Tea key names onto the names tmux send-keys knows.
var tmuxKeyNames = map[string]string{
	"enter": "Enter", "esc": "Escape", "escape": "Escape",
	"backspace": "BSpace", "delete": "DC", "insert": "IC",
	"tab": "Tab", "shift+tab": "BTab",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"home": "Home", "end": "End", "pgup": "PPage", "pgdown": "NPage",
}

// tmuxKey translates a keypress into what send-keys needs: a literal string,
// or a tmux key name. ok is false for keys tmux has no name for, which are
// dropped rather than guessed at.
func tmuxKey(msg tea.KeyMsg) (key string, literal bool, ok bool) {
	switch msg.Type {
	case tea.KeyRunes:
		return string(msg.Runes), true, true
	case tea.KeySpace:
		return " ", true, true
	}
	s := msg.String()
	if named, found := tmuxKeyNames[s]; found {
		return named, false, true
	}
	if rest, found := strings.CutPrefix(s, "ctrl+"); found && len(rest) == 1 {
		return "C-" + rest, false, true
	}
	if rest, found := strings.CutPrefix(s, "alt+"); found && len(rest) == 1 {
		return "M-" + rest, false, true
	}
	return "", false, false
}

const cursorFormat = "#{cursor_x},#{cursor_y},#{cursor_flag}"

// CaptureAgent reads a pane's screen and its cursor position in one tmux call.
// The live agent view runs this several times a second, and one fork per frame
// instead of two is the difference between typing that echoes and typing that
// trails.
//
// The cursor is asked for first so it is always the one line before the
// capture. The other way round the boundary would have to be guessed, and a
// pane can print anything - including a line that looks like a cursor report.
//
// A cursor that will not parse fails the whole frame. Reading it separately
// used to swallow that error and draw the screen anyway, which left the block
// sitting wherever it was last seen; a live view has to be trusted about where
// your next character is going.
func (t tmuxClient) CaptureAgent(ctx context.Context, tgt string) ([]string, cursorPos, error) {
	out, err := t.r.run(ctx, "",
		"tmux", "display-message", "-p", "-t", tgt, cursorFormat,
		";", "capture-pane", "-e", "-p", "-t", tgt)
	if err != nil {
		return nil, cursorPos{}, err
	}
	// Split always yields at least one element, so the cursor line is there
	// even for empty output - parseCursor is what rejects it.
	all := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	cur, err := parseCursor(all[0])
	if err != nil {
		return nil, cursorPos{}, err
	}
	return trimBlankTail(all[1:]), cur, nil
}

// cursorPos is where the cursor sits in a pane and whether it is visible at
// all - a full-screen program can hide it. Coordinates are pane-relative and
// zero-based.
type cursorPos struct {
	x, y    int
	visible bool
}

func parseCursor(line string) (cursorPos, error) {
	f := strings.Split(strings.TrimSpace(line), ",")
	if len(f) != 3 {
		return cursorPos{}, fmt.Errorf("tmux: unexpected cursor format %q", line)
	}
	x, err := strconv.Atoi(f[0])
	if err != nil {
		return cursorPos{}, err
	}
	y, err := strconv.Atoi(f[1])
	if err != nil {
		return cursorPos{}, err
	}
	return cursorPos{x: x, y: y, visible: f[2] == "1"}, nil
}

func trimBlankTail(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(stripANSI(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ---------------------------------------------------------------------------
// Keystroke forwarding
// ---------------------------------------------------------------------------

// keystroke is one key on its way to a pane, already translated into what
// send-keys wants.
type keystroke struct {
	target  string
	key     string
	literal bool
}

// keyRun is consecutive keystrokes that a single send-keys can carry: same
// target, and either all literal or all named, because -l is a flag for the
// whole command rather than a per-key one.
type keyRun struct {
	target  string
	literal bool
	keys    []string
}

// keySender forwards keystrokes to tmux from one goroutine.
//
// send-keys used to run inline in Update. Bubble Tea runs commands in
// concurrent goroutines, so issuing it as a command raced - typing
// "echo hello" arrived as "echohello" when the space overtook the word before
// it - and running it inline instead cost the event loop a fork per character.
// A single consumer draining a FIFO gives the ordering without the stall, and
// coalescing whatever queued up during the previous call means a burst costs
// one tmux invocation rather than one per byte.
type keySender struct {
	tmux tmuxClient
	keys chan keystroke
	errs chan error
}

func newKeySender(t tmuxClient) *keySender {
	s := &keySender{
		tmux: t,
		keys: make(chan keystroke, 512),
		errs: make(chan error, 1),
	}
	go s.pump()
	return s
}

// send queues a keystroke and returns immediately. It drops the key rather
// than block: a full queue means tmux has been wedged for seconds, and waiting
// there would freeze the whole dashboard instead of just the one pane. The
// drop is reported, never silent - a keystroke that vanishes without a trace
// is the one failure a thing you type into must not have.
func (s *keySender) send(k keystroke) {
	select {
	case s.keys <- k:
	default:
		s.fail(fmt.Errorf("tmux: input backed up, dropped %q", k.key))
	}
}

// fail reports the first failure to reach it and discards any piled up behind
// it. The dashboard has one notice line, so a queue of them would do nothing
// but overwrite each other.
func (s *keySender) fail(err error) {
	select {
	case s.errs <- err:
	default:
	}
}

func (s *keySender) pump() {
	for k := range s.keys {
		// A pane that has gone away fails every run aimed at it, so the rest
		// of the burst headed there is abandoned. Runs for other targets are
		// not: coalesce splits by target, and a burst spans two panes when
		// keys are still draining as the live view moves to another session.
		var dead string
		for _, run := range coalesce(append([]keystroke{k}, s.drain()...)) {
			if run.target == dead {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := s.tmux.SendKeys(ctx, run)
			cancel()
			if err != nil {
				dead = run.target
				s.fail(err)
			}
		}
	}
}

// drain takes everything queued right now without waiting for more.
func (s *keySender) drain() []keystroke {
	var out []keystroke
	for {
		select {
		case k := <-s.keys:
			out = append(out, k)
		default:
			return out
		}
	}
}

func coalesce(ks []keystroke) []keyRun {
	var runs []keyRun
	for _, k := range ks {
		if n := len(runs); n > 0 && runs[n-1].target == k.target && runs[n-1].literal == k.literal {
			runs[n-1].keys = append(runs[n-1].keys, k.key)
			continue
		}
		runs = append(runs, keyRun{target: k.target, literal: k.literal, keys: []string{k.key}})
	}
	return runs
}
