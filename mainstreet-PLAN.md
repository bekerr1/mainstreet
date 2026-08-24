# mainstreet

A launcher and viewer for a tmux-based, agent-heavy development environment.

One small Go binary that is two things:

- a **TUI** for browsing and attaching to sessions
- a **CLI** for agents, so an orchestrator agent can spawn and drive work

It runs on the machine whose tmux it drives. That is the whole architecture.

---

## Vision

Development moves onto a personal VPS. Everything durable — tmux, treehouse
worktrees, firstmate crews, the coding agents themselves — lives there. A laptop
and a phone become thin clients, and the thing they are thin clients *of* is
SSH.

The gap today is that there is no way to *see* the fleet. `tmux ls` is a flat
list with no worktree context, no window detail, and no way to go from "I want a
session on the auth bug" to a fully laid-out session without several manual
steps. mainstreet is that missing layer: the front door to the fleet.

The second goal is agentic: the same commands the TUI runs are exposed as a
clean JSON CLI, so a persistent orchestrator agent can create sessions, lease
worktrees, and launch processes on request.

## Non-goals

- **Not a terminal multiplexer.** tmux owns sessions, windows and panes.
- **Not a worktree manager.** [treehouse](https://github.com/kunchenguid/treehouse) owns the worktree pool.
- **Not an agent orchestrator.** [firstmate](https://github.com/kunchenguid/firstmate) owns crews and supervision.
- **Not a web app.** No browser terminal, no HTTP server, no websockets. A phone
  runs an SSH client and gets the real TUI.
- **Not a remote-control client.** mainstreet never talks to another host. It is
  always local to the tmux it drives.

mainstreet is the thin layer that composes these and gives them one front door.

---

## How this relates to what already exists

| Tool | Owns | mainstreet's relationship |
| --- | --- | --- |
| tmux | sessions, windows, panes | reads and drives it; never replaces it |
| treehouse | pool of pre-warmed git worktrees | calls `status --json`, `get --lease`, `return` |
| firstmate | orchestrator agent + autonomous crew, each in a worktree | shows its sessions and `fm-task*` windows alongside your own; `o` attaches to it |

**On firstmate specifically.** It already dispatches a crew of agents, gives
each a clean treehouse worktree, and uses tmux as its default backend. There is
no overlap in practice: firstmate is a workflow that lives *inside* one tmux
session; mainstreet is the browser over *all* sessions. The "orchestrator
terminal" in this design most likely just *is* firstmate's first mate, and `o`
in the TUI attaches to it.

**On running firstmate remotely.** Its `docs/remote-secondmates.md` feature is
not the way to get firstmate onto a VPS. That feature places a whole persistent
firstmate *home* on another host — it explicitly does not support placing an
individual worker remotely — it always runs on the experimental Herdr backend,
and Herdr's remote-session server belongs to the host's GUI login session, so
the remote box needs a desktop session. It also never runs a login shell there,
so shell config is bypassed.

The right model is far simpler, and it is the same one mainstreet uses: **run it
on the VPS directly.** Its tmux backend has no locality or GUI assumptions; SSH
in and attach and it behaves exactly as it does locally.

---

## Architecture

The binary is always local to the tmux it drives. SSH is how you reach the
machine, not something the program knows about.

```
laptop ──ssh──► VPS
                 ├── tmux server
                 │     ├── session: mainstreet   ← the TUI lives here
                 │     ├── session: live-expo
                 │     └── session: bugfix-441
                 ├── treehouse worktree pool
                 └── firstmate crew
```

Consequences, all of them simplifications:

- **Attach hands over the terminal, and detaching comes back.** mainstreet is
  meant to run in its own terminal, *not* inside a tmux pane: `↵` execs
  `tmux attach -t <session>` as a child process, and detaching (prefix-d) ends
  the child and repaints the dashboard. That is the loop the tool exists for,
  and it is also exactly what `ssh -t vps mainstreet` gives you.

  When mainstreet *is* run inside tmux (`$TMUX` set), a nested attach is not
  possible, so it falls back to `switch-client`. That works, but detaching then
  leaves tmux entirely rather than returning here - the degraded mode, not the
  intended one.
- **No connection layer.** No SSH backend, no ControlMaster tuning, no daemon,
  no tunnel, no token, no HTTP surface. There is nothing to authenticate
  because there is nothing listening.
- **tmux does the wire optimization.** A Bubble Tea repaint over a laggy tether
  would be unpleasant on its own; inside tmux it is tmux's problem, and tmux is
  very good at it.
- **The phone story is free.** Any SSH client (Blink, Termius) runs the real
  TUI — real terminal, real key handling, zero code.

The one thing this gives up is a single view over more than one host. That is
the only honest reason to ever build a remote backend, and it stays unbuilt
until it is actually missed. Running the same binary locally covers the
occasional laptop-tmux case without any new code.

---

## Stack

Small on purpose. Two direct dependencies.

| Concern | Choice | Why |
| --- | --- | --- |
| Language | **Go** | One static binary, `scp` to deploy, no runtime on the VPS. Matches treehouse and no-mistakes. |
| TUI | **bubbletea** + **lipgloss** | Elm loop, alt-screen handling, and — the part that matters — correct key/escape-sequence parsing. lipgloss for colour and borders. |
| CLI | **stdlib `flag` + a `switch`** | ~10 subcommands does not need cobra. |
| Config | **hardcoded defaults**, TOML later | See "Session layouts" below. |

**Deliberately not used:**

- **bubbles** — the list/viewport components pull the model toward their shape.
  The layout here is fixed and rendering it is about sixty lines.
- **cobra** — a large dependency to replace `switch os.Args[1]`. Completions
  are not worth it for a personal tool.
- **A config-file parser, at first** — the default layout is a Go slice until
  there is a second profile that needs to differ.
- **creack/pty, websockets, xterm.js, embed.FS** — all were phase-6 web
  machinery. Gone with it.

Going lower than bubbletea (raw `golang.org/x/term`) is a false economy: you
immediately reimplement escape-sequence parsing for arrow keys and modifiers,
which is fiddly and is most of what bubbletea is buying.

---

## Repo layout

Flat `package main`. Five files, no `internal/` tree. Since it is one package,
file boundaries here are *navigational, not structural* — nothing is enforced
and nothing is encapsulated, so the right number is however many make things
findable. Five nouns you would actually grep for beats nine with two stub files
in it.

```
mainstreet/
├── main.go        # flag parsing, subcommand dispatch, JSON output, exit codes
├── tmux.go        # the runner seam, every tmux command, the default layout
├── treehouse.go   # status --json, get --lease, return
├── state.go       # State/Session/Window/Worktree types, the one gather call
├── tui.go         # bubbletea Model/Update/View, modals, kanagawa palette
├── PLAN.md
└── README.md
```

Rough budget: ~1200 lines total, with `tui.go` the largest at ~450. **Split
`tui.go` when it passes ~500 lines**, and the natural seam is the modals
(`tui_modal.go`) — but split it then, not now.

Things that were separate files in the previous draft and are not any more: the
`runner` seam (~30 lines, top of `tmux.go`), the colour palette (~15 lines, top
of `tui.go`), and the layout profile (~50 lines, next to the tmux code that is
its only consumer).

### The one abstraction

Instead of a wide `Backend` interface, the seam sits at the process boundary,
where it is naturally narrow:

```go
type runner interface {
    run(ctx context.Context, argv ...string) ([]byte, error)
}
```

`tmux.go` and `treehouse.go` hold one of these. Production passes an
`exec.CommandContext` implementation; tests pass a fake that returns canned
`tmux list-sessions` output. That is the entire testing story, and it covers
the parsing logic — which is where the bugs actually are — without an interface
per subsystem.

tmux output is read with explicit `-F` format strings and a delimiter, so
parsing stays a `strings.Split`. treehouse already emits JSON. Neither needs a
dependency.

---

## TUI design

```
 mainstreet ──────────────────────────────────────────────── vps:dev-01
SESSIONS (11)         │ live-expo
───────────────────── │ ──────────────────────────────────────────────
› live-expo           │ dir      ~/dev/live-expo    0 agent   claude ●
  custom-model-hosting│ created  Aug 22 10:41       1 base    bash
  me-dev-config     ● │ activity now ago            2 build   npm
  outreach-agent      │ clients  1 attached         3 src     nvim
  agent-evals         │
  envoy               │  ● live  live-expo:0
  llmc                │ ──────────────────────────────────────────────
  my-agent            │   ⏺ Updated src/auth/session.ts
                      │
                      │   ────────────────────────────────────────────
                      │   ❯ run the tests again█
                      │   ────────────────────────────────────────────
                      │     ⏵⏵ accept edits on
───────────────────────────────────────────────────────────────────────
 ↵ attach  a live agent  n new  d kill  w worktrees  / filter  q quit
```

The session list is **just names**. Window counts and ages were columns there
once; they are already in the detail pane, and a narrow list leaves more width
for the agent, which is the thing actually being read. The column sizes itself
to the longest name. A single `●` marks an attached session.

The detail pane puts the session stats and its window list on the same rows, so
the agent preview below them gets the full remaining height.

The worktree pool is hidden until `w` asks for it. While hidden its treehouse
call is skipped entirely, so moving the cursor costs nothing.

`n` — the name starts empty and has to be typed; it used to be pre-filled from
the working directory, which made "confirm a name you didn't choose" the
common case. `tab` moves to a directory field: type any part of a path under
mainstreet's cwd and it fuzzy-matches subdirectories, ranked and scanned with
two things learned from testing it against a real, large `~/Development`:

- **Matching is against a directory's own name first**, not its full path.
  Scoring the whole path let letters from unrelated ancestors chain into an
  accidental match - "proj" found something four levels inside
  `go-control-plane` before it ever found a real top-level `projects`
  directory. A shallower directory also outranks a deeper one at equal match
  quality, on the theory that what you want to `cd` into is usually a project
  itself, not something buried inside one. A query that only matches an
  ancestor segment still falls back to a full-path match, just scored low
  enough that a real name match always wins.
- **The scan is breadth-first, not depth-first**, and capped at 3 levels. A
  depth-first walk spends its whole results budget on the first huge tree it
  meets - a vendored checkout, a Go module cache - before ever reaching a
  sibling top-level directory, so the directory you actually wanted might
  never even become a candidate. Level-by-level scanning guarantees every
  top-level directory is scanned before any of them goes a level deeper, so
  one big sibling can only crowd out its *own* deep results.

The top match is highlighted as you type so `↵` just works; `↑↓` picks a
different one. Nothing highlighted and nothing typed means the session lands
in cwd itself - the query is never silently defaulted to the first
suggestion. A query past the scan depth is still accepted, taken literally,
so a directory the scanner didn't reach can still be typed out in full.

`N` — lease a worktree and build a laid-out session in one step:

```
┌─ new session from worktree ───────────────────────┐
│  worktree   › 2  bugfix-441   (free)              │
│               3  (empty)      (free)              │
│  name       bugfix-441_                           │
│  layout     default  (0:base 1:build 2:src …)     │
│                                                   │
│  will run:                                        │
│    treehouse get --lease --lease-holder bugfix-…  │
│    tmux new-session -d -s bugfix-441 -c <path>    │
│    + 9 windows                                    │
│                                                   │
│  ↵ create   esc cancel                            │
└───────────────────────────────────────────────────┘
```

The detail pane shows **the agent window's current screen**, refreshed every
couple of seconds.

`a` makes that pane **live** *in place*: the session list and stats stay exactly
where they are, the pane re-captures several times a second, and every keystroke
is forwarded to the agent with `send-keys`. `ctrl+]` returns focus to the list.
Nothing is attached, detached, or switched - and if you want the agent full
size, that is what attaching to the session is for. Because tmux drives a window that is not its session's
current one, the target session is not disturbed at all: its active window is
exactly where you left it.

Two constraints shaped this:

- **No `join-pane`.** tmux can physically move an agent's pane into another
  window, which would give a real terminal with no polling. It needs mainstreet
  to be running inside tmux, and mainstreet deliberately is not - see Attach
  above. Capture-and-forward is the approach that survives that decision.
- **Keys are sent synchronously.** Bubble Tea runs commands in concurrent
  goroutines, so a burst of keystrokes issued as commands races and arrives out
  of order - typing "echo hello" produced "echohello". A `send-keys` is a few
  milliseconds and ordering is worth blocking for.

The cursor is drawn from `#{cursor_x}`/`#{cursor_y}`/`#{cursor_flag}` as one
cell of reverse video, counted in *visible* columns so colour escapes in the
line do not shift it. When it sits past the end of a line - the usual case,
just after a prompt - the line is padded and the block drawn on a space.

Known limit: captured lines are as wide as the *source* pane, so a dashboard
narrower than the agent's terminal truncates. It is the reason to look at the
dashboard at all: you can see which project is mid-task and which is sitting at
a prompt without attaching to any of them. The agent window is found by name
(`agent`, then `claude`, the older convention) and failing that by what is
running in it.

### Keys

| Key | Action |
| --- | --- |
| `↵` | attach (child process; detach returns here) |
| `a` | **live agent** - drive the agent window from inside the dashboard |
| `A` | attach straight into the agent window (`select-window`, then attach) |
| `ctrl+]` | leave live agent mode |
| `n` | new session in the current directory |
| `N` | new session from a treehouse worktree |
| `d` | kill session (confirm; asks about the worktree if leased) |
| `o` | attach to the orchestrator session |
| `w` | show or hide the worktree pool |
| `/` | filter |
| `R` | refresh |
| `tab` | move between the sessions and worktrees panes |
| `q` | quit |

Colours follow the kanagawa-dragon palette used by the `me` nvim and tmux
configs so the whole environment reads as one system. `theme.go` is a handful
of `lipgloss.Color` constants — no theming system.

---

## Session layouts

The default profile, in `tmux.go` as a Go slice:

```go
var defaultLayout = []layoutWindow{
	{0, "agent"},
	{1, "base"},
	{2, "build"},
	{3, "src"},
}
```

The agent leads at index 0 because reaching it is usually the reason you came,
and tmux selects a session's first window on attach - so `↵` on a fresh session
lands you at the agent.

Window 0 cannot be created directly: `base-index` is 1 in the me tmux config, so
tmux puts a new session's first window at 1 and mainstreet moves it to 0. This
is what "window 0 is created explicitly" means in practice. `renumber-windows
off` is what keeps it from sliding back on the first close.

A TOML config at `~/.config/me/mainstreet.toml` was in the original plan. It is
deferred until a second profile actually exists.

---

## Agent CLI surface

Every TUI action has a non-interactive equivalent. This is what makes the
orchestrator work: the agent's tool is the same binary.

```sh
mainstreet state --json                                # sessions + windows + worktrees, one call
mainstreet session list --json
mainstreet session new <name> [--worktree N | --path P] [--layout default] --json
mainstreet session kill <name> [--return-worktree]
mainstreet session attach <name>                       # interactive
mainstreet window new <session> --index 3 --name spare [--cmd '...']
mainstreet run <session>:<window> -- <cmd>             # send-keys into a window
mainstreet worktree list --json
mainstreet worktree lease [--holder X] --json
mainstreet worktree return <path> [--if-lease-id ID]
```

Rules: every mutating command is idempotent where it can be, prints JSON with
`--json`, sends banners to stderr so stdout stays parseable, and returns a
non-zero exit with a structured error on failure.

These get documented in the `me` repo's `agents/AGENTS.md` so every harness
knows them without being told.

---

## Milestones

Each phase is independently useful; stop at any point and still have something
worth running.

### Phase 0 — skeleton
Go module, subcommand dispatch, the `runner` seam, `tmux.go` reading real
sessions.
**Done when:** `mainstreet state --json` prints the real local tmux session list.

### Phase 1 — read-only TUI
Two-pane layout, sessions list, window detail, treehouse pane, refresh, filter.
**Done when:** every running session, its windows, and the worktree pool render
accurately and match `tmux ls` / `treehouse status`.

### Phase 2 — attach and lifecycle
`↵` attach as a child process, `n` new session on the default layout, `d` kill
with confirmation, a live preview of the selected session's agent window, and
`a` to drive that agent without leaving the dashboard. Rename was dropped as
not worth a key.
**Done when:** a full day's session management needs no raw tmux commands.

### Phase 3 — layouts and treehouse
The default layout, `N` = lease + create + lay out, kill with optional worktree
return.
**Done when:** one keypress goes from a free worktree to a laid-out session.

### Phase 4 — agent CLI
The full command surface above, JSON everywhere, AGENTS.md documentation.
**Done when:** an orchestrator agent creates a session on a worktree from a
natural-language request and it appears in the TUI.

### Phase 5 — VPS
Cross-compile (`GOOS=linux GOARCH=amd64 go build .`), `scp` to the
box, a dedicated `mainstreet` tmux session that autostarts the TUI, and a
laptop alias that is just `ssh -t vps tmux attach -t mainstreet`.
**Done when:** closing the laptop lid loses nothing, and the same alias from a
phone's SSH client works identically.

There is no phase 6. The mobile goal — "a phone can list sessions and attach" —
is delivered by phase 5 plus an SSH client.

---

## Open decisions

Recorded rather than blocking. Defaults are what will be implemented unless
changed.

1. **Window `0` and the gap at `6`** — assumed deliberate; default layout above
   reflects it verbatim.
2. **Killing a session with a leased worktree** — default is to *prompt*.
   `treehouse return` resets the worktree, so silent auto-return can destroy
   uncommitted work. An alternative is to auto-return only when `git status` is
   clean.
3. **Orchestrator identity** — default assumption is that it is the `firstmate`
   session and `o` simply attaches to it. If firstmate does not end up being the
   orchestrator, this becomes a configurable session name.
4. **~~Invocation name~~** — settled. The repo, module, and binary are all
   `mainstreet`, so `go build .` needs no `-o` and what is on the VPS matches
   what the docs say. A `main` shell alias is a later, purely local
   convenience — it belongs in shell config, not in the build. The tmux session
   hosting the TUI is also `mainstreet`.

## Prerequisites for the VPS transition

Not blockers for phases 0-4, which run fine against local tmux.

- Agent auth (Claude, Codex, Pi) must live on the VPS. That is credentials on a
  rented box — a real change in blast radius.
- Sizing: several concurrent agents plus builds plus nvim. Firstmate crews are
  genuinely parallel; this wants real CPU and RAM, not 1 vCPU.
- `gh auth login` on the VPS, since firstmate and no-mistakes both need it.
- A reliable SSH setup, since it is now the only access path: key auth, a
  `~/.ssh/config` host entry, and mosh worth considering for flaky links.
