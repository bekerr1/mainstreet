package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type modalKind int

const (
	modalNone modalKind = iota
	modalNewSession
	modalConfirmKill
)

// modalField is which field of the new-session form has focus. Tab cycles
// between them.
type modalField int

const (
	fieldName modalField = iota
	fieldDir
)

func (f modalField) other() modalField {
	if f == fieldName {
		return fieldDir
	}
	return fieldName
}

// modalState is the whole of mainstreet's modal UI. There are two of them and
// neither needs a text-input component: a name field plus a fuzzy-filtered
// directory picker, and a yes/no.
type modalState struct {
	kind  modalKind
	input string // session name being typed
	field modalField

	// baseDir is mainstreet's own working directory - fixed for the life of
	// the modal. dirQuery narrows it: a session lands in baseDir itself when
	// the query is empty, or in baseDir/<selection> otherwise. dirOptions is
	// scanned once when the modal opens; dirMatches is dirOptions filtered
	// against dirQuery, recomputed on every keystroke.
	baseDir    string
	dirQuery   string
	dirOptions []string
	dirMatches []string
	// dirCursor indexes dirMatches. -1 means nothing is highlighted, in which
	// case Enter falls back to dirQuery taken literally - which is what makes
	// "nothing typed yet" mean "just use baseDir" rather than silently
	// picking the first match.
	dirCursor int

	target string // session a confirm applies to
	detail string // second line of a confirm
	danger bool   // confirm needs a stronger colour
	err    string
}

func (s modalState) active() bool { return s.kind != modalNone }

// resolvedDir is where the session will actually be created.
func (s modalState) resolvedDir() string {
	sub := s.selectedSubdir()
	if sub == "" {
		return s.baseDir
	}
	return filepath.Join(s.baseDir, sub)
}

// selectedSubdir is the highlighted fuzzy match, or - if nothing is
// highlighted - whatever was typed, taken literally. The literal fallback
// matters past the scan depth: a directory the scan did not reach still
// works to type out in full, it is just not offered as a suggestion.
func (s modalState) selectedSubdir() string {
	if s.dirCursor >= 0 && s.dirCursor < len(s.dirMatches) {
		return s.dirMatches[s.dirCursor]
	}
	return strings.Trim(s.dirQuery, "/")
}

type actionMsg struct{ err error }

type attachDoneMsg struct{ err error }

func newSessionCmd(t tmuxClient, name, dir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return actionMsg{err: t.NewSession(ctx, name, dir, defaultLayout)}
	}
}

func killSessionCmd(t tmuxClient, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return actionMsg{err: t.KillSession(ctx, name)}
	}
}

func switchClientCmd(t tmuxClient, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return attachDoneMsg{err: t.SwitchClient(ctx, name)}
	}
}

// openNewSession starts the form empty: the name used to be pre-filled from
// the working directory, which meant confirming a name you had not chosen was
// the common case. Scanning subdirectories happens once, here, rather than on
// every keystroke in the dir field.
func (m *model) openNewSession() {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	options := scanSubdirs(dir)
	m.modal = modalState{
		kind:       modalNewSession,
		baseDir:    dir,
		dirOptions: options,
		dirMatches: filterDirs(options, ""),
		dirCursor:  -1,
	}
}

func (m *model) openConfirmKill(s Session) {
	detail := fmt.Sprintf("%d windows will be terminated", s.WindowCount)
	if s.Name == m.self {
		detail = "this is the session mainstreet is running in"
	}
	m.modal = modalState{
		kind:   modalConfirmKill,
		target: s.Name,
		detail: detail,
		danger: s.Name == m.self,
	}
}

func (m model) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.modal.kind {
	case modalNewSession:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEsc:
			m.modal = modalState{}
			return m, nil
		case tea.KeyTab, tea.KeyShiftTab:
			m.modal.field = m.modal.field.other()
			return m, nil
		case tea.KeyEnter:
			name := strings.TrimSpace(m.modal.input)
			if name == "" {
				m.modal.err = "a session needs a name"
				return m, nil
			}
			if m.sessionExists(name) {
				m.modal.err = fmt.Sprintf("%q already exists", name)
				return m, nil
			}
			dir := m.modal.resolvedDir()
			m.modal = modalState{}
			return m, newSessionCmd(m.tmux, name, dir)
		case tea.KeyUp:
			if m.modal.field == fieldDir && m.modal.dirCursor > -1 {
				m.modal.dirCursor--
			}
			return m, nil
		case tea.KeyDown:
			if m.modal.field == fieldDir && m.modal.dirCursor < len(m.modal.dirMatches)-1 {
				m.modal.dirCursor++
			}
			return m, nil
		case tea.KeyBackspace:
			switch m.modal.field {
			case fieldName:
				if m.modal.input != "" {
					m.modal.input = m.modal.input[:len(m.modal.input)-1]
				}
			case fieldDir:
				if m.modal.dirQuery != "" {
					m.modal.dirQuery = m.modal.dirQuery[:len(m.modal.dirQuery)-1]
				}
				m.modal.refilterDirs()
			}
			m.modal.err = ""
			return m, nil
		case tea.KeyRunes:
			switch m.modal.field {
			case fieldName:
				// tmux treats : and . as target separators, so they cannot
				// appear in a session name.
				for _, r := range msg.Runes {
					if r != ':' && r != '.' {
						m.modal.input += string(r)
					}
				}
			case fieldDir:
				m.modal.dirQuery += string(msg.Runes)
				m.modal.refilterDirs()
			}
			m.modal.err = ""
			return m, nil
		}
		return m, nil

	case modalConfirmKill:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "y", "Y", "enter":
			name := m.modal.target
			m.modal = modalState{}
			return m, killSessionCmd(m.tmux, name)
		case "n", "N", "esc", "q":
			m.modal = modalState{}
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

// refilterDirs recomputes dirMatches for the current query. A non-empty query
// with hits highlights the top one, so Enter accepts it without an extra
// keypress; an empty query - or one with no hits - deselects, so Enter falls
// through to resolvedDir's baseDir default instead of a stale highlight.
func (s *modalState) refilterDirs() {
	s.dirMatches = filterDirs(s.dirOptions, s.dirQuery)
	if s.dirQuery != "" && len(s.dirMatches) > 0 {
		s.dirCursor = 0
	} else {
		s.dirCursor = -1
	}
}

// fieldValue renders one text field: the caret only appears on whichever
// field currently has focus, and an unfocused empty field shows a dim
// placeholder rather than looking blank and possibly abandoned.
func (s modalState) fieldValue(f modalField, value, placeholder string) string {
	if s.field != f {
		if value == "" {
			return styleDim.Render(placeholder)
		}
		return styleRow.Render(value)
	}
	return styleSelected.Render(value + "▏")
}

// dirPickerRows lists the current fuzzy matches under the dir field, with the
// highlighted one marked the same way the session list marks its selection.
// It renders nothing when the dir field does not have focus, so the form
// stays compact until you actually go looking for a directory.
func (s modalState) dirPickerRows() []string {
	if s.field != fieldDir {
		return nil
	}
	if len(s.dirMatches) == 0 {
		return []string{styleDim.Render("          no matching directories")}
	}
	rows := make([]string, len(s.dirMatches))
	for i, d := range s.dirMatches {
		if i == s.dirCursor {
			rows[i] = styleSelected.Render("        › " + d)
		} else {
			rows[i] = styleDim.Render("          " + d)
		}
	}
	return rows
}

func detailStyle(danger bool) lipgloss.Style {
	if danger {
		return styleErr
	}
	return styleDim
}

// ---------------------------------------------------------------------------
// Directory picker
//
// The new-session dir field lets you type a subdirectory of mainstreet's cwd
// - (cwd)/<here> - rather than only ever creating sessions in cwd itself.
// Options are scanned once per modal open; fuzzy filtering is cheap enough to
// run on every keystroke.
// ---------------------------------------------------------------------------

const (
	maxScanDepth   = 3    // levels below cwd worth offering as suggestions
	maxScanResults = 4000 // safety valve; BFS below means this is spent on
	// breadth before depth, so it does not starve out whole top-level dirs
	maxDirMatches = 8 // rows the picker list actually has room for
)

// skipDirNames are the entries a project tree is full of that are never what
// you meant to cd into.
var skipDirNames = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, "__pycache__": true, "venv": true, ".venv": true,
}

// scanSubdirs walks root and returns every subdirectory found within
// maxScanDepth levels, as a path relative to root. Hidden directories, the
// noise in skipDirNames, and symlinks are skipped - the last because
// os.DirEntry reports a symlink's own type rather than its target's, which
// conveniently means the walk never follows one into a loop.
//
// The walk is breadth-first, level by level, on purpose: a depth-first walk
// spends the whole maxScanResults budget on the first huge tree it meets - a
// vendored checkout, a Go module cache - before ever reaching a sibling
// top-level directory, so the directory you actually wanted is never even a
// candidate. BFS guarantees every top-level directory is scanned before any
// of them goes a level deeper, so a big sibling can only crowd out *its own*
// deep results, never another project's.
func scanSubdirs(root string) []string {
	var out []string
	level := []string{""} // relative paths to expand next; "" is root itself
	for depth := 1; depth <= maxScanDepth && len(level) > 0 && len(out) < maxScanResults; depth++ {
		var next []string
		for _, rel := range level {
			if len(out) >= maxScanResults {
				break
			}
			dir := root
			if rel != "" {
				dir = filepath.Join(root, rel)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if len(out) >= maxScanResults {
					break
				}
				name := e.Name()
				if e.Type()&fs.ModeSymlink != 0 || !e.IsDir() ||
					strings.HasPrefix(name, ".") || skipDirNames[name] {
					continue
				}
				child := name
				if rel != "" {
					child = rel + "/" + name
				}
				out = append(out, child)
				next = append(next, child)
			}
		}
		level = next
	}
	sort.Strings(out)
	return out
}

// filterDirs ranks options against a fuzzy query, most relevant first, capped
// to what the picker can show. An empty query returns options in scan order
// (shallow and alphabetical) as a plain browse list.
func filterDirs(options []string, query string) []string {
	if query == "" {
		if len(options) > maxDirMatches {
			return options[:maxDirMatches]
		}
		return options
	}
	type scored struct {
		path  string
		score int
	}
	matches := make([]scored, 0, len(options))
	for _, opt := range options {
		if score, ok := fuzzyScore(query, opt); ok {
			matches = append(matches, scored{opt, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	if len(matches) > maxDirMatches {
		matches = matches[:maxDirMatches]
	}
	out := make([]string, len(matches))
	for i, sc := range matches {
		out[i] = sc.path
	}
	return out
}

// fuzzyScore matches primarily against candidate's own name (the last path
// segment), not the full path. Matching the full path let letters from
// unrelated ancestor directories chain together into an accidental match -
// "proj" found go-control-plane/**/*proj* four levels deep in a directory
// nobody was looking for, while never surfacing a real top-level "projects"
// folder that the results cap had crowded out. A shallower directory then
// outranks a deeper one at equal match quality, since the thing you actually
// want to cd into is usually a project itself, not something buried inside
// one.
//
// A query that only matches an ancestor segment - "envoy main" for
// envoy/envoy-main - still falls back to a full-path match, just scored low
// enough that a real basename match always wins.
func fuzzyScore(query, candidate string) (int, bool) {
	base := candidate
	if i := strings.LastIndexByte(candidate, '/'); i >= 0 {
		base = candidate[i+1:]
	}
	if score, ok := subsequenceScore(query, base); ok {
		depth := strings.Count(candidate, "/")
		return score*10 - depth, true
	}
	return subsequenceScore(query, candidate)
}

// subsequenceScore reports whether every rune of query appears in candidate in
// order, case-insensitively, and a score favouring consecutive runs and
// matches right after a path separator or word boundary.
func subsequenceScore(query, candidate string) (int, bool) {
	q := []rune(strings.ToLower(query))
	c := []rune(strings.ToLower(candidate))
	score, qi, run := 0, 0, 0
	for ci := 0; ci < len(c) && qi < len(q); ci++ {
		if c[ci] != q[qi] {
			run = 0
			continue
		}
		boundary := ci == 0 || c[ci-1] == '/' || c[ci-1] == '-' || c[ci-1] == '_'
		run++
		points := 1 + run
		if boundary {
			points += 3
		}
		score += points
		qi++
	}
	if qi < len(q) {
		return 0, false
	}
	return score, true
}

func (m model) sessionExists(name string) bool {
	for _, s := range m.state.Sessions {
		if s.Name == name {
			return true
		}
	}
	return false
}

// modalView renders the dialog centred in the body area. mainstreet does not
// composite an overlay: the modal replaces the panes while it is up, which is
// less code and leaves no doubt about what has focus.
func (m model) modalView(width, height int) string {
	var title string
	var rows []string

	switch m.modal.kind {
	case modalNewSession:
		title = "new session"
		names := make([]string, 0, len(defaultLayout))
		for _, w := range defaultLayout {
			names = append(names, fmt.Sprintf("%d:%s", w.Index, w.Name))
		}
		rows = append(rows,
			styleLabel.Render("name    ")+m.modal.fieldValue(fieldName, m.modal.input, "(required)"),
			styleLabel.Render("dir     ")+stylePath.Render(fit(baseName(m.modal.baseDir), 20))+"/"+
				m.modal.fieldValue(fieldDir, m.modal.dirQuery, "(cwd)"),
		)
		rows = append(rows, m.modal.dirPickerRows()...)
		rows = append(rows,
			styleLabel.Render("windows ")+styleWindow.Render(strings.Join(names, "  ")),
			"",
			styleKey.Render("tab")+styleKeyHint.Render(" field   ")+
				styleKey.Render("↑↓")+styleKeyHint.Render(" pick   ")+
				styleKey.Render("↵")+styleKeyHint.Render(" create   ")+
				styleKey.Render("esc")+styleKeyHint.Render(" cancel"),
		)
	case modalConfirmKill:
		title = "kill session"
		rows = []string{
			styleRow.Render("kill ") + styleSelected.Render(m.modal.target) + styleRow.Render("?"),
			detailStyle(m.modal.danger).Render(m.modal.detail),
			"",
			styleKey.Render("y") + styleKeyHint.Render(" kill   ") +
				styleKey.Render("n/esc") + styleKeyHint.Render(" cancel"),
		}
	}
	if m.modal.err != "" {
		rows = append(rows, styleErr.Render(m.modal.err))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colFaint).
		Padding(0, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			append([]string{styleTitle.Render(title), ""}, rows...)...))

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}
