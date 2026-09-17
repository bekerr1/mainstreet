package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFitTruncatesAndPads(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w    int
		want string
	}{
		{"envoy", 8, "envoy   "},
		{"custom-model-hosting", 10, "custom-mo…"},
		{"exact", 5, "exact"},
		{"x", 1, "x"},
		{"xy", 1, "…"},
		{"anything", 0, ""},
	} {
		if got := fit(tc.in, tc.w); got != tc.want {
			t.Errorf("fit(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}
}

func TestScrollStartKeepsCursorVisible(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		cursor, rows, total, want int
	}{
		{"everything fits", 3, 20, 5, 0},
		{"cursor at top", 0, 5, 20, 0},
		{"cursor centred", 10, 5, 20, 8},
		{"cursor at end clamps", 19, 5, 20, 15},
		{"no rows", 4, 0, 20, 0},
	} {
		if got := scrollStart(tc.cursor, tc.rows, tc.total); got != tc.want {
			t.Errorf("%s: scrollStart(%d,%d,%d) = %d, want %d",
				tc.name, tc.cursor, tc.rows, tc.total, got, tc.want)
		}
	}
}

func TestVisibleFiltersOnNameAndDir(t *testing.T) {
	m := model{state: State{Sessions: []Session{
		{Name: "envoy", Dir: "/home/b/Development/envoy/envoy-main"},
		{Name: "my-agent", Dir: "/home/b/Development/projects/agents-explore"},
		{Name: "llmc", Dir: "/home/b/Development/llmc/llm.c"},
	}}}

	if got := len(m.visible()); got != 3 {
		t.Errorf("no filter should show everything, got %d", got)
	}

	// A session's name can lie about what it is working on, so the filter
	// matches the directory too: "explore" finds my-agent.
	m.filter = "explore"
	got := m.visible()
	if len(got) != 1 || got[0].Name != "my-agent" {
		t.Errorf("dir match failed, got %+v", got)
	}

	m.filter = "ENVOY" // case-insensitive
	if got := m.visible(); len(got) != 1 || got[0].Name != "envoy" {
		t.Errorf("case-insensitive name match failed, got %+v", got)
	}

	m.filter = "nothing-matches"
	if got := m.visible(); len(got) != 0 {
		t.Errorf("want no matches, got %d", len(got))
	}
}

func TestActiveDirPrefersActiveWindow(t *testing.T) {
	windows := []Window{
		{Index: 0, Path: "/home/b/repo", Active: false},
		{Index: 9, Path: "/home/b/repo/agent", Active: true},
	}
	if got := activeDir(windows); got != "/home/b/repo/agent" {
		t.Errorf("activeDir = %q, want the active window's path", got)
	}
	// tmux always marks one window active, but a session mid-teardown may not.
	if got := activeDir([]Window{{Path: "/home/b/repo"}}); got != "/home/b/repo" {
		t.Errorf("activeDir with no active flag should fall back, got %q", got)
	}
	if got := activeDir(nil); got != "" {
		t.Errorf("activeDir(nil) = %q, want empty", got)
	}
}

func TestAttachAgentReportsMissingAgentWindow(t *testing.T) {
	m := model{state: State{Sessions: []Session{
		{Name: "scratch", Windows: []Window{{Index: 0, Name: "bash", Command: "bash"}}},
	}}}
	got, cmd := m.attachAgent()
	if cmd != nil {
		t.Error("want no command when there is nothing to attach to")
	}
	// The notice, not err: the refresh tick owns err and would wipe this
	// before it could be read.
	if n := got.(model).notice; n != "scratch has no agent window" {
		t.Errorf("notice = %q, want it to name the session", n)
	}
}

func TestAnsiFitCountsVisibleColumnsOnly(t *testing.T) {
	// A colour escape is 5+ bytes and zero columns. fit() would count those
	// bytes and slice the escape in half, spilling control characters.
	line := "\x1b[32mgreen\x1b[0m and more text"
	got := ansiFit(line, 9)
	if visible := stripANSI(got); visible != "green and" {
		t.Errorf("visible text = %q, want %q", visible, "green and")
	}
	if !strings.Contains(got, "\x1b[32m") {
		t.Errorf("colour escape was dropped: %q", got)
	}

	if got := ansiFit("plain text here", 5); got != "plain" {
		t.Errorf("plain truncation = %q, want %q", got, "plain")
	}
	if got := ansiFit("anything", 0); got != "" {
		t.Errorf("zero width = %q, want empty", got)
	}
}

func TestWithCursorMarksTheRightColumn(t *testing.T) {
	got := withCursor("abcdef", 2)
	if stripANSI(got) != "abcdef" {
		t.Errorf("text changed: %q", stripANSI(got))
	}
	if !strings.Contains(got, "\x1b[7mc\x1b[27m") {
		t.Errorf("cursor should sit on 'c', got %q", got)
	}

	// The usual case: the cursor sits one past the end of the line, just after
	// a prompt. The line is padded out and the block drawn on a space.
	past := withCursor("abc", 5)
	if visible := stripANSI(past); visible != "abc   " {
		t.Errorf("padded line = %q, want %q", visible, "abc   ")
	}

	// Escape sequences must not shift the column: the cursor is counted in
	// visible cells, so here it lands on 'd', not somewhere inside the escape.
	coloured := withCursor("\x1b[32mab\x1b[0mcd", 3)
	if !strings.Contains(coloured, "\x1b[7md\x1b[27m") {
		t.Errorf("cursor should sit on 'd', got %q", coloured)
	}
}

func TestListWidthTracksLongestName(t *testing.T) {
	short := model{state: State{Sessions: []Session{{Name: "llmc"}}}}
	if got := short.listWidth(); got != 20 {
		t.Errorf("short names should hit the floor, got %d", got)
	}
	long := model{state: State{Sessions: []Session{
		{Name: "llmc"}, {Name: "custom-model-hosting"},
	}}}
	if got := long.listWidth(); got != 26 {
		t.Errorf("width = %d, want longest name + 6", got)
	}
	huge := model{state: State{Sessions: []Session{
		{Name: strings.Repeat("x", 80)},
	}}}
	if got := huge.listWidth(); got != 34 {
		t.Errorf("a runaway name should hit the ceiling, got %d", got)
	}
}

func TestWorktreesAreNotFetchedWhileHidden(t *testing.T) {
	sessions := []Session{{Name: "proj", Dir: "/repo",
		Windows: []Window{{Index: 0, Name: "bash", Command: "bash", Active: true}}}}

	hidden := model{state: State{Sessions: sessions}}
	hidden.syncSelection(false)
	if hidden.wtDir != "" {
		t.Errorf("hidden pane should not trigger a treehouse call, wtDir = %q", hidden.wtDir)
	}

	shown := model{state: State{Sessions: sessions}, showWorktrees: true}
	shown.syncSelection(false)
	if shown.wtDir != "/repo" {
		t.Errorf("shown pane should fetch for the selection, wtDir = %q", shown.wtDir)
	}
}

func TestScanSubdirsSkipsNoiseHiddenAndSymlinks(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root,
		"envoy/envoy-main",
		"envoy/envoy-slack-invite",
		"live-expo/live-expo-main",
		"live-expo/node_modules/some-pkg",
		".hidden-dir",
		"llmc/llm.c/.git",
		"deep/a/b/c/d", // 4 levels: deeper than maxScanDepth should reach
	)
	if err := os.Symlink(root, filepath.Join(root, "a-symlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := scanSubdirs(root)
	want := map[string]bool{
		"envoy": true, "envoy/envoy-main": true, "envoy/envoy-slack-invite": true,
		"live-expo": true, "live-expo/live-expo-main": true,
		"llmc": true, "llmc/llm.c": true,
		"deep": true, "deep/a": true, "deep/a/b": true,
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("scanSubdirs returned %q, which should have been filtered", g)
		}
		delete(want, g)
	}
	if len(want) != 0 {
		t.Errorf("scanSubdirs missed: %v", want)
	}
}

func mkdirs(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
}

func TestFilterDirsRanksBoundaryMatchesFirst(t *testing.T) {
	options := []string{
		"envoy/envoy-main",
		"envoy/envoy-slack-invite",
		"live-expo/live-expo-main",
		"llmc/llm.c",
		"my-agent",
	}

	if got := filterDirs(options, ""); len(got) != len(options) {
		t.Errorf("empty query should return everything (under the cap), got %v", got)
	}

	// "em" should favour envoy-main over envoy-slack-invite: 'm' lands right
	// after a path separator in one and mid-word in the other.
	got := filterDirs(options, "em")
	if len(got) == 0 || got[0] != "envoy/envoy-main" {
		t.Errorf("filterDirs(\"em\") top match = %v, want envoy/envoy-main first", got)
	}

	if got := filterDirs(options, "zzz-nope"); len(got) != 0 {
		t.Errorf("no subsequence match should return nothing, got %v", got)
	}
}

func TestModalStateResolvedDir(t *testing.T) {
	base := "/home/b/Development"

	// Nothing typed: the session lands in cwd itself, not in whatever
	// happens to be first in the scan.
	empty := modalState{baseDir: base, dirCursor: -1}
	if got := empty.resolvedDir(); got != base {
		t.Errorf("empty state resolvedDir = %q, want baseDir %q", got, base)
	}

	// A highlighted fuzzy match wins.
	picked := modalState{
		baseDir: base, dirQuery: "env",
		dirMatches: []string{"envoy/envoy-main", "envoy/envoy-slack-invite"},
		dirCursor:  0,
	}
	if got := picked.resolvedDir(); got != filepath.Join(base, "envoy/envoy-main") {
		t.Errorf("resolvedDir = %q, want the highlighted match joined to baseDir", got)
	}

	// Typed text past the scan depth, with nothing highlighted, is taken
	// literally rather than rejected.
	literal := modalState{baseDir: base, dirQuery: "some/very/deep/path", dirCursor: -1}
	if got := literal.resolvedDir(); got != filepath.Join(base, "some/very/deep/path") {
		t.Errorf("resolvedDir = %q, want the literal query joined to baseDir", got)
	}
}

func TestRefilterDirsResetsCursor(t *testing.T) {
	s := modalState{dirOptions: []string{"envoy/envoy-main", "live-expo/live-expo-main"}}

	s.dirQuery = "envoy"
	s.refilterDirs()
	if s.dirCursor != 0 {
		t.Errorf("a query with hits should highlight the top match, dirCursor = %d", s.dirCursor)
	}

	s.dirQuery = ""
	s.refilterDirs()
	if s.dirCursor != -1 {
		t.Errorf("clearing the query should deselect, dirCursor = %d", s.dirCursor)
	}

	s.dirQuery = "zzz-nope"
	s.refilterDirs()
	if s.dirCursor != -1 {
		t.Errorf("a query with no hits should deselect, dirCursor = %d", s.dirCursor)
	}
}

// liveModel is a dashboard sitting in the live agent view, with a capture
// already in flight - the steady state the poll loop runs in.
func liveModel() model {
	return model{
		state: State{Sessions: []Session{{
			Name: "evals",
			Dir:  "/w",
			Windows: []Window{
				{Index: 0, Name: "agent", Active: true, Command: "claude", Path: "/w"},
			},
		}}},
		agentFocus:    true,
		previewTarget: "evals:0",
		capturing:     "evals:0",
	}
}

// The live view must stay exactly one poll loop however many things ask for a
// frame. It used to fork: the 2s refresh forced a capture of its own, every
// capture booked a fresh tick, and nothing ever retired the old one - so the
// capture rate climbed for as long as you sat there, and typing crawled.
func TestAgentPollStaysOneLoop(t *testing.T) {
	m := liveModel()

	// A frame arrives. It books the next tick, and exactly one.
	next, cmd := m.Update(previewMsg{target: "evals:0"})
	m = next.(model)
	if m.capturing != "" {
		t.Fatalf("frame did not clear the in-flight capture, got %q", m.capturing)
	}
	if !m.pollQueued || cmd == nil {
		t.Fatal("frame did not book the next poll")
	}

	// The refresh tick lands mid-loop. It may not start a capture of its own,
	// and it may not book a second tick.
	next, cmd = m.Update(stateMsg{st: m.state})
	m = next.(model)
	if m.capturing != "" {
		t.Errorf("refresh forked a second capture of %q", m.capturing)
	}
	if cmd != nil {
		t.Error("refresh scheduled work while the agent was live")
	}
	if !m.pollQueued {
		t.Error("refresh retired the queued poll")
	}

	// Nor may a second frame, whatever produced it.
	if _, cmd = m.Update(previewMsg{target: "evals:0"}); cmd != nil {
		t.Error("a second frame booked a second poll")
	}

	// The tick fires: one capture goes out, and the tick is spent.
	next, cmd = m.Update(agentPollMsg{})
	m = next.(model)
	if m.pollQueued {
		t.Error("a fired tick still counts as queued")
	}
	if m.capturing != "evals:0" || cmd == nil {
		t.Fatalf("tick did not capture, capturing=%q", m.capturing)
	}

	// Typing wants a frame too, but not a duplicate of the one in flight.
	if _, cmd = m.Update(echoMsg{}); cmd != nil {
		t.Error("echo captured on top of a capture already in flight")
	}
}

// Leaving the live view has to end the loop, or it polls a pane nobody is
// looking at for the rest of the session.
func TestAgentPollStopsWhenFocusLeaves(t *testing.T) {
	m := liveModel()
	m.agentFocus = false

	if _, cmd := m.Update(previewMsg{target: "evals:0"}); cmd != nil {
		t.Error("the loop outlived the focus")
	}
}

// A frame for a pane we have moved on from is stale, but the loop still has to
// come out the other side of it.
func TestStalePreviewKeepsTheLoopAlive(t *testing.T) {
	m := liveModel()
	m.previewTarget = "other:0"

	next, cmd := m.Update(previewMsg{target: "evals:0", lines: []string{"stale"}})
	m = next.(model)
	if m.preview != nil {
		t.Errorf("stale frame was drawn: %q", m.preview)
	}
	if !m.pollQueued || cmd == nil {
		t.Error("stale frame stranded the loop")
	}
}

// Typing echoes off the pane, so a keystroke has to pull a frame of its own
// rather than wait out the idle poll.
func TestTypingAsksForAFrame(t *testing.T) {
	m := liveModel()
	m.keys = newKeySender(tmuxClient{r: &recordingRunner{}})
	m.capturing = ""

	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}); cmd == nil {
		t.Error("a keystroke scheduled no redraw")
	}
}
