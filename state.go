package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"
)

// State is the whole picture in one value: what `mainstreet state --json`
// prints and what the TUI renders. Gathering it is a single call so the CLI
// and the TUI can never drift.
type State struct {
	Host      string     `json:"host"`
	Sessions  []Session  `json:"sessions"`
	Worktrees []Worktree `json:"worktrees,omitempty"`
	// Warnings carries degraded-but-usable conditions (treehouse missing, a
	// pool that is not a repo). They never fail the call: a machine without
	// treehouse still has sessions worth showing.
	Warnings []string `json:"warnings,omitempty"`
}

type Session struct {
	Name        string    `json:"name"`
	WindowCount int       `json:"window_count"`
	Dir         string    `json:"dir"`
	Created     time.Time `json:"created"`
	Activity    time.Time `json:"activity"`
	Attached    int       `json:"attached"`
	Windows     []Window  `json:"windows,omitempty"`
}

type Window struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Active  bool   `json:"active"`
	Command string `json:"command"`
	Path    string `json:"path"`
}

type Worktree struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Branch  string `json:"branch,omitempty"`
	Status  string `json:"status,omitempty"`
	Leased  bool   `json:"leased"`
	Holder  string `json:"holder,omitempty"`
	LeaseID string `json:"lease_id,omitempty"`
}

func gather(ctx context.Context, t tmuxClient) (State, error) {
	sessions, err := t.Sessions(ctx)
	if err != nil {
		return State{}, err
	}
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	return State{Host: host, Sessions: sessions}, nil
}

// printState renders the human-readable form. It is deliberately plain: the
// TUI is where the design lives, and this exists so `mainstreet state` piped
// to a pager is still readable.
func printState(w io.Writer, st State) error {
	if len(st.Sessions) == 0 {
		_, err := fmt.Fprintln(w, "no tmux sessions")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tWINDOWS\tATTACHED\tACTIVITY\tDIR")
	for _, s := range st.Sessions {
		attached := ""
		if s.Attached > 0 {
			attached = "yes"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
			s.Name, s.WindowCount, attached, since(s.Activity), s.Dir)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, warn := range st.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warn)
	}
	return nil
}

// since formats an age the way the TUI's activity column does: now, 4m, 3h, 2d.
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
