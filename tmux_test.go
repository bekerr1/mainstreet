package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
type recordingRunner struct {
	calls   [][]string
	windows string
	capture string
}

func (r *recordingRunner) run(_ context.Context, _ string, argv ...string) ([]byte, error) {
	r.calls = append(r.calls, argv)
	switch argv[1] {
	case "list-windows":
		return []byte(r.windows), nil
	case "capture-pane":
		return []byte(r.capture), nil
	}
	return nil, nil
}

func (r *recordingRunner) find(sub string) []string {
	for _, c := range r.calls {
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
