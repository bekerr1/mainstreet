package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeRunner answers tmux calls from canned output, so these tests exercise the
// parsing without a tmux server. Fields are joined with the real sep, which
// keeps the fixtures honest about the delimiter.
type fakeRunner struct {
	sessions    string
	sessionsErr error
	windows     map[string]string
}

func (f fakeRunner) run(_ context.Context, _ string, argv ...string) ([]byte, error) {
	switch argv[1] {
	case "list-sessions":
		if f.sessionsErr != nil {
			return nil, f.sessionsErr
		}
		return []byte(f.sessions), nil
	case "list-windows":
		target := argv[3]
		out, ok := f.windows[target]
		if !ok {
			return nil, fmt.Errorf("can't find session: %s", target)
		}
		return []byte(out), nil
	}
	return nil, fmt.Errorf("unexpected tmux call: %v", argv)
}

func row(fields ...string) string { return strings.Join(fields, sep) + "\n" }

func TestSessionsParsesWindowsAndTimestamps(t *testing.T) {
	r := fakeRunner{
		sessions: row("agent-evals", "4", "1786201724", "1786807598", "0") +
			row("live-expo", "1", "1787414496", "1787414496", "2"),
		windows: map[string]string{
			"agent-evals": row("0", "bash", "1", "bash", "/home/b/agent-evals") +
				row("9", "claude", "0", "claude", "/home/b/agent-evals"),
			"live-expo": row("0", "bash", "1", "nvim", "/home/b/live-expo"),
		},
	}

	got, err := tmuxClient{r: r}.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}

	ae := got[0]
	if ae.Name != "agent-evals" || ae.WindowCount != 4 || ae.Attached != 0 {
		t.Errorf("session header wrong: %+v", ae)
	}
	if ae.Created.Unix() != 1786201724 {
		t.Errorf("created = %d, want 1786201724", ae.Created.Unix())
	}
	if len(ae.Windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(ae.Windows))
	}
	// The gap between 0 and 9 is the point: renumber-windows is off, so an
	// index is not a position in the slice.
	if ae.Windows[1].Index != 9 || ae.Windows[1].Name != "claude" {
		t.Errorf("second window = %+v, want index 9 named claude", ae.Windows[1])
	}
	if !ae.Windows[0].Active || ae.Windows[1].Active {
		t.Errorf("active flags wrong: %+v", ae.Windows)
	}
	if got[1].Attached != 2 {
		t.Errorf("live-expo attached = %d, want 2", got[1].Attached)
	}
}

func TestSessionsEmptyWhenNoServer(t *testing.T) {
	r := fakeRunner{sessionsErr: errors.New("no server running on /tmp/tmux-1000/default")}
	got, err := tmuxClient{r: r}.Sessions(context.Background())
	if err != nil {
		t.Fatalf("a stopped tmux server is a normal empty state, got error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d sessions, want none", len(got))
	}
}

func TestSessionsRejectsMalformedLine(t *testing.T) {
	// What a control-character separator produced: tmux escaped it, so the
	// whole record arrived as a single field.
	r := fakeRunner{sessions: "agent-evals\\0374\\037/home/b\\0371786201724\n"}
	if _, err := (tmuxClient{r: r}).Sessions(context.Background()); err == nil {
		t.Fatal("want an error naming the field count, got nil")
	} else if !strings.Contains(err.Error(), "5 session fields") {
		t.Errorf("error should say what it expected, got: %v", err)
	}
}

// recordingRunner captures the argv of every tmux call so tests can assert on
// the commands mainstreet builds. This is how the mutating operations get
// covered: creating and killing sessions for real in a test would touch the
// developer's actual tmux server.
//
// It is mutex-guarded because the keystroke sender calls tmux from its own
// goroutine, which is the whole reason it exists.
type recordingRunner struct {
	mu      sync.Mutex
	calls   [][]string
	windows string
	capture string
}

func (r *recordingRunner) run(_ context.Context, _ string, argv ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	switch argv[1] {
	case "list-windows":
		return []byte(r.windows), nil
	// A combined cursor-then-capture reads as display-message; both answer
	// from the same fixture.
	case "capture-pane", "display-message":
		return []byte(r.capture), nil
	}
	return nil, nil
}

func (r *recordingRunner) argv() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

func (r *recordingRunner) find(sub string) []string {
	for _, c := range r.argv() {
		if len(c) > 1 && c[1] == sub {
			return c
		}
	}
	return nil
}

func TestNewSessionMovesFirstWindowToIndexZero(t *testing.T) {
	// base-index is 1 in the me config, so tmux puts the session's first
	// window at 1 and it has to be moved to occupy index 0.
	rec := &recordingRunner{windows: row("1", "agent", "1", "bash", "/p")}
	if err := (tmuxClient{r: rec}).NewSession(
		context.Background(), "proj", "/p", defaultLayout); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if got := rec.find("new-session"); !contains(got, "-s", "proj") || !contains(got, "-n", "agent") {
		t.Errorf("new-session argv wrong: %v", got)
	}
	move := rec.find("move-window")
	if !contains(move, "-s", "proj:1") || !contains(move, "-t", "proj:0") {
		t.Errorf("want the first window moved 1 -> 0, got: %v", move)
	}

	var created []string
	for _, c := range rec.calls {
		if c[1] == "new-window" {
			created = append(created, c[argIndex(c, "-t")+1]+"="+c[argIndex(c, "-n")+1])
		}
	}
	want := []string{"proj:1=base", "proj:2=build", "proj:3=src"}
	if len(created) != len(want) {
		t.Fatalf("created %v, want %v", created, want)
	}
	for i := range want {
		if created[i] != want[i] {
			t.Errorf("window %d = %s, want %s", i, created[i], want[i])
		}
	}

	// The agent window is what you came for, so it must end up selected.
	if sel := rec.find("select-window"); !contains(sel, "-t", "proj:0") {
		t.Errorf("want agent window selected, got: %v", sel)
	}
}

func TestNewSessionSkipsMoveWhenAlreadyAtZero(t *testing.T) {
	rec := &recordingRunner{windows: row("0", "agent", "1", "bash", "/p")}
	if err := (tmuxClient{r: rec}).NewSession(
		context.Background(), "proj", "/p", defaultLayout); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if move := rec.find("move-window"); move != nil {
		t.Errorf("no move needed when base-index is 0, got: %v", move)
	}
}

func TestAgentWindowPreference(t *testing.T) {
	cases := []struct {
		name    string
		windows []Window
		want    int
		found   bool
	}{
		{"prefers a window named agent", []Window{
			{Index: 0, Name: "agent"}, {Index: 9, Name: "claude", Command: "claude"},
		}, 0, true},
		{"falls back to claude, the older convention", []Window{
			{Index: 0, Name: "bash"}, {Index: 9, Name: "claude"},
		}, 9, true},
		{"falls back to what is running", []Window{
			{Index: 0, Name: "bash", Command: "bash"},
			{Index: 3, Name: "work", Command: "codex"},
		}, 3, true},
		{"no agent at all", []Window{{Index: 0, Name: "bash", Command: "bash"}}, 0, false},
	}
	for _, tc := range cases {
		got, ok := agentWindow(Session{Windows: tc.windows})
		if ok != tc.found {
			t.Errorf("%s: found = %v, want %v", tc.name, ok, tc.found)
			continue
		}
		if ok && got.Index != tc.want {
			t.Errorf("%s: picked window %d, want %d", tc.name, got.Index, tc.want)
		}
	}
}

func TestCapturePaneTrimsTrailingBlankLines(t *testing.T) {
	rec := &recordingRunner{capture: "❯ waiting for input\n\n\n\n"}
	got, err := (tmuxClient{r: rec}).CapturePane(context.Background(), "proj:0")
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	// A tmux pane is padded to its full height; without trimming, the preview
	// would be mostly blank lines and the prompt would scroll out of view.
	if len(got) != 1 || got[0] != "❯ waiting for input" {
		t.Errorf("got %q, want the single non-blank line", got)
	}
}

func TestAttachArgv(t *testing.T) {
	want := []string{"tmux", "attach", "-t", "envoy"}
	got := attachArgv("envoy")
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func argIndex(argv []string, flag string) int {
	for i, a := range argv {
		if a == flag {
			return i
		}
	}
	return -1
}

func contains(argv []string, flag, value string) bool {
	i := argIndex(argv, flag)
	return i >= 0 && i+1 < len(argv) && argv[i+1] == value
}

func TestTmuxKeyMapping(t *testing.T) {
	cases := []struct {
		name    string
		msg     tea.KeyMsg
		key     string
		literal bool
		ok      bool
	}{
		{"runes go literally", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")}, "hi", true, true},
		// Space is its own key type in Bubble Tea, not a rune, and losing it
		// silently mangles every command typed into an agent.
		{"space", tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}, " ", true, true},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, "Enter", false, true},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, "BSpace", false, true},
		{"escape", tea.KeyMsg{Type: tea.KeyEsc}, "Escape", false, true},
		{"arrows", tea.KeyMsg{Type: tea.KeyUp}, "Up", false, true},
		{"shift+tab", tea.KeyMsg{Type: tea.KeyShiftTab}, "BTab", false, true},
		{"ctrl", tea.KeyMsg{Type: tea.KeyCtrlC}, "C-c", false, true},
		{"unmapped keys are dropped, not guessed", tea.KeyMsg{Type: tea.KeyF5}, "", false, false},
	}
	for _, tc := range cases {
		key, literal, ok := tmuxKey(tc.msg)
		if ok != tc.ok || key != tc.key || literal != tc.literal {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)",
				tc.name, key, literal, ok, tc.key, tc.literal, tc.ok)
		}
	}
}

func TestStripANSI(t *testing.T) {
	if got := stripANSI("\x1b[32mgreen\x1b[0m"); got != "green" {
		t.Errorf("stripANSI = %q, want %q", got, "green")
	}
}

// sentKeys reconstructs the key stream from recorded send-keys calls, so a
// burst split across calls reads the same as one that was not.
func sentKeys(calls [][]string) []string {
	var out []string
	for _, argv := range calls {
		if len(argv) < 5 || argv[1] != "send-keys" {
			continue
		}
		if argv[4] == "-l" {
			for _, r := range argv[5] {
				out = append(out, string(r))
			}
			continue
		}
		out = append(out, argv[4:]...)
	}
	return out
}

func TestCoalesceGroupsWhatOneSendKeysCanCarry(t *testing.T) {
	runs := coalesce([]keystroke{
		{target: "s:0", key: "h", literal: true},
		{target: "s:0", key: "i", literal: true},
		{target: "s:0", key: "Enter"},
		{target: "s:0", key: "Up"},
		{target: "s:0", key: "x", literal: true},
		{target: "other:0", key: "y", literal: true},
	})

	want := []keyRun{
		{target: "s:0", literal: true, keys: []string{"h", "i"}},
		{target: "s:0", keys: []string{"Enter", "Up"}},
		{target: "s:0", literal: true, keys: []string{"x"}},
		{target: "other:0", literal: true, keys: []string{"y"}},
	}
	if len(runs) != len(want) {
		t.Fatalf("got %d runs, want %d: %+v", len(runs), len(want), runs)
	}
	for i := range want {
		if runs[i].target != want[i].target || runs[i].literal != want[i].literal ||
			strings.Join(runs[i].keys, ",") != strings.Join(want[i].keys, ",") {
			t.Errorf("run %d = %+v, want %+v", i, runs[i], want[i])
		}
	}
}

// -l takes the whole run as one argument; named keys go one per argument.
func TestSendKeysArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  keyRun
		want string
	}{
		{"literal run joins", keyRun{target: "s:0", literal: true, keys: []string{"h", "i"}},
			"tmux send-keys -t s:0 -l hi"},
		{"named keys stay apart", keyRun{target: "s:0", keys: []string{"Enter", "Up"}},
			"tmux send-keys -t s:0 Enter Up"},
	} {
		rec := &recordingRunner{}
		if err := (tmuxClient{r: rec}).SendKeys(context.Background(), tc.run); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Join(rec.argv()[0], " "); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The sender exists to keep bursts in order without stalling the event loop.
// Typing "echo hello" once arrived as "echohello", because the space overtook
// the word in front of it.
func TestKeySenderPreservesOrder(t *testing.T) {
	rec := &recordingRunner{}
	s := newKeySender(tmuxClient{r: rec})

	var want []string
	for _, r := range "echo hello" {
		s.send(keystroke{target: "s:0", key: string(r), literal: true})
		want = append(want, string(r))
	}
	s.send(keystroke{target: "s:0", key: "Enter"})
	want = append(want, "Enter")

	var got []string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if got = sentKeys(rec.argv()); len(got) == len(want) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Errorf("tmux saw %q, want %q", got, want)
	}
}

// The cursor is read in the same call as the capture, on the line in front of
// it - a pane can print anything, including a line that looks like a cursor.
func TestCaptureAgentReadsCursorThenScreen(t *testing.T) {
	rec := &recordingRunner{capture: "7,2,1\n$ ls\n56,24,1\n\n\n"}

	lines, cur, err := (tmuxClient{r: rec}).CaptureAgent(context.Background(), "s:0")
	if err != nil {
		t.Fatal(err)
	}
	if cur != (cursorPos{x: 7, y: 2, visible: true}) {
		t.Errorf("cursor = %+v, want {7 2 true}", cur)
	}
	if strings.Join(lines, "|") != "$ ls|56,24,1" {
		t.Errorf("lines = %q", lines)
	}

	// One tmux invocation, not two.
	if n := len(rec.argv()); n != 1 {
		t.Errorf("made %d tmux calls, want 1", n)
	}
}

// A cursor tmux will not give us fails the frame rather than drawing the
// screen with a stale block on it.
func TestCaptureAgentRejectsAnUnreadableCursor(t *testing.T) {
	for _, capture := range []string{"", "not-a-cursor\n$ ls\n", "7,2\n$ ls\n"} {
		rec := &recordingRunner{capture: capture}
		if _, _, err := (tmuxClient{r: rec}).CaptureAgent(context.Background(), "s:0"); err == nil {
			t.Errorf("capture %q was accepted", capture)
		}
	}
}

// failingRunner refuses one target and accepts every other.
type failingRunner struct {
	recordingRunner
	bad string
}

func (f *failingRunner) run(ctx context.Context, dir string, argv ...string) ([]byte, error) {
	out, err := f.recordingRunner.run(ctx, dir, argv...)
	for _, a := range argv {
		if a == f.bad {
			return nil, fmt.Errorf("can't find pane: %s", f.bad)
		}
	}
	return out, err
}

// A burst spans two panes when keys are still draining as the live view moves
// on. One dead pane must not swallow the keys bound for the live one.
func TestKeySenderDoesNotDropOtherTargetsOnFailure(t *testing.T) {
	rec := &failingRunner{bad: "dead:0"}
	s := newKeySender(tmuxClient{r: rec})

	s.send(keystroke{target: "dead:0", key: "a", literal: true})
	s.send(keystroke{target: "live:0", key: "b", literal: true})

	var got []string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if got = sentKeys(rec.argv()); len(got) == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if strings.Join(got, "") != "ab" {
		t.Errorf("tmux saw %q, want both keys attempted", got)
	}

	select {
	case err := <-s.errs:
		if !strings.Contains(err.Error(), "dead:0") {
			t.Errorf("reported %v, want the dead pane", err)
		}
	case <-time.After(time.Second):
		t.Error("the failure was never reported")
	}
}

// A keystroke that cannot be queued has to say so. Silently eating input is
// the failure mode the whole live view exists to avoid.
func TestKeySenderReportsDroppedKeys(t *testing.T) {
	s := &keySender{keys: make(chan keystroke), errs: make(chan error, 1)} // no pump, so nothing drains
	s.send(keystroke{target: "s:0", key: "x", literal: true})

	select {
	case err := <-s.errs:
		if !strings.Contains(err.Error(), "dropped") {
			t.Errorf("reported %v, want a drop", err)
		}
	default:
		t.Error("a dropped keystroke was swallowed")
	}
}
