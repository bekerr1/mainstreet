# mainstreet

A lean, fast TUI front door to a tmux-based, agent-heavy dev environment.

`mainstreet` sits next to `tmux`, `treehouse`, and `firstmate`, and lets
you see every session at a glance and drop straight into whichever agent
needs you, without leaving the dashboard to find out.

```
 mainstreet ────────────────────────────────────────────── b3k3r-ubuntu1
SESSIONS (12)       │ agent-evals
────────────────────│───────────────────────────────────────────────────
› agent-evals       │ dir      ~/Development/projects/agent-evals-te…
  applied-ai        │ created  Aug 8 11:08
  custom-model-…    │ activity 7d ago
  live-expo         │ clients  none
  llmc              │
  mainstreet     ●  │ AGENT  9:claude
  me-dev-config-…●  │───────────────────────────────────────────────────
  my-agent          │   openai:chat:gpt-4o-mini reads as "call OpenAI's
  nvidia-stuff      │   Chat Completions endpoint with that model."
  outreach-agent    │
                    │   ✻ Cogitated for 26s
                    │
                    │───────────────────────────────────────────────────
                    │ ❯
                    │───────────────────────────────────────────────────
                    │   ⏵⏵ accept edits on (shift+tab to cycle)
────────────────────────────────────────────────────────────────────────
 ↵ attach  a live agent  n new  d kill  w worktrees  / filter  q quit
```

## Why

`tmux ls` is a flat list. It tells you nothing about which session is stuck
waiting on you and which one is still thinking. Attaching to check is slow
enough that you stop doing it, which means the agent that needed you five
minutes ago is still sitting there.

`mainstreet` fixes that by putting the thing you actually want to know — what
is this session's agent doing *right now* — one keystroke away, without
touching tmux's attach/detach state at all.

## The workflow

1. Open `mainstreet`. The left column is every tmux session, name only.
2. Move down the list. The right pane updates: directory, timestamps, window
   list, and a live-ish preview of that session's agent window.
3. See one waiting on you? Press **`a`**. The preview goes live — keystrokes
   you type are forwarded straight into that window, with a real blinking
   cursor — while the session list and stats stay exactly where they are.
4. Done? **`ctrl+]`** drops you back to the list. Nothing was attached,
   nothing was detached, nothing on the target session moved.
5. Want the agent full-screen instead? **`↵`** attaches for real. Detaching
   (tmux's own prefix-`d`) brings you straight back to `mainstreet`.

That's the whole tool. Everything else — new sessions, killing sessions, the
treehouse worktree pool — is one key away but out of sight until you ask.

## Install

```sh
go build .
```

One static binary, two dependencies (`bubbletea`, `lipgloss`). No config file,
no daemon, nothing to install alongside it beyond `tmux` itself and
(optionally) [`treehouse`](https://github.com/kunchenguid/treehouse) for the
worktree pool.

Run it from a plain terminal, not from inside a tmux pane — that's what makes
attach/detach round-trip back to the dashboard instead of leaving tmux
entirely.

## Keys

| Key | Does |
| --- | --- |
| `↑↓` / `j`/`k` | move the selection |
| `g` / `G` | jump to top / bottom |
| `a` | make the agent preview **live**, in place — keys forward, cursor moves |
| `ctrl+]` | leave live mode, back to the session list |
| `↵` | attach for real (full screen); detach comes back here |
| `A` | attach and land directly in the agent window |
| `n` | new session — `0:agent 1:base 2:build 3:src`, named from cwd |
| `d` | kill session, with confirmation (and a louder warning if it's this one) |
| `w` | show or hide the treehouse worktree pool for the selected session |
| `tab` | move focus between the session list and the worktree pool |
| `/` | filter sessions by name or working directory |
| `R` | refresh now |
| `q` | quit |

## How the live agent view works

There's no tmux magic under the hood — mainstreet runs *outside* tmux on
purpose, so detaching from a real attach can return to it. So `a` is built
from three plain primitives:

- `tmux capture-pane -e` — the screen, colors included
- `tmux send-keys` — your keystrokes, forwarded and sent synchronously so a
  fast typing burst can't arrive out of order
- `tmux display-message '#{cursor_x},#{cursor_y},#{cursor_flag}'` — drawn as
  one cell of reverse video, counted in visible columns so it doesn't land
  inside a color escape

None of it requires the target session's *active* window to be the agent
window, which is what makes it safe: mainstreet can be driving a window that
isn't even the one the session would show you if you attached normally.

## What it is not

- **Not a multiplexer.** tmux owns sessions, windows, and panes.
- **Not a worktree manager.** [treehouse](https://github.com/kunchenguid/treehouse)
  owns the pool; mainstreet just shows it and, later, leases from it.
- **Not an orchestrator.** [firstmate](https://github.com/kunchenguid/firstmate)
  owns crews; mainstreet is the window onto whatever they're doing.
- **Not a web app.** No server, no browser terminal. A phone gets there over
  SSH, running the same binary.

See [`mainstreet-PLAN.md`](./mainstreet-PLAN.md) for the full design, the
phase-by-phase roadmap, and the decisions recorded along the way.

## Status

Phases 0–2 of the plan: reading real tmux state, the two-pane TUI, session
lifecycle (attach / new / kill), and the live agent view above. Next up is
leasing treehouse worktrees straight into a new session.
