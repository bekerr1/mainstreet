package main

import (
	"context"
	"fmt"
	"os"
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

// modalState is the whole of mainstreet's modal UI. There are two of them and
// neither needs a text-input component: a name field and a yes/no.
type modalState struct {
	kind   modalKind
	input  string // session name being typed
	dir    string // where a new session will be created
	target string // session a confirm applies to
	detail string // second line of a confirm
	danger bool   // confirm needs a stronger colour
	err    string
}

func (s modalState) active() bool { return s.kind != modalNone }

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

// openNewSession seeds the name from the working directory, since sessions are
// created from the project you are standing in.
func (m *model) openNewSession() {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	m.modal = modalState{kind: modalNewSession, input: baseName(dir), dir: dir}
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
			dir := m.modal.dir
			m.modal = modalState{}
			return m, newSessionCmd(m.tmux, name, dir)
		case tea.KeyBackspace:
			if m.modal.input != "" {
				m.modal.input = m.modal.input[:len(m.modal.input)-1]
			}
			m.modal.err = ""
			return m, nil
		case tea.KeyRunes:
			// tmux treats : and . as target separators, so they cannot appear
			// in a session name.
			for _, r := range msg.Runes {
				if r != ':' && r != '.' {
					m.modal.input += string(r)
				}
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

func detailStyle(danger bool) lipgloss.Style {
	if danger {
		return styleErr
	}
	return styleDim
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
		rows = []string{
			styleLabel.Render("name    ") + styleSelected.Render(m.modal.input+"▏"),
			styleLabel.Render("dir     ") + stylePath.Render(fit(m.modal.dir, 44)),
			styleLabel.Render("windows ") + styleWindow.Render(strings.Join(names, "  ")),
			"",
			styleKey.Render("↵") + styleKeyHint.Render(" create   ") +
				styleKey.Render("esc") + styleKeyHint.Render(" cancel"),
		}
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
