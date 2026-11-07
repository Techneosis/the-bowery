# The Bowery

*A courier network for coding agents. Everyone sent out is known by name,
every errand is written down, and everyone comes home.*

The Bowery sends AI coding agents out into [herdr](https://herdr.dev): a pane
of their own, on this machine or on a shared box across town, with the job in
hand and the bell switched off. When the job is done it brings them home and
clears away everything that was put out for them: the pane, the tab, the
worktree, the branch. Nothing more. It was built for orchestrators: agents
that hire other agents mid-conversation and, left to themselves, forget who
they sent where.

```
bowery dispatch reviewer --worktree review-412 --cwd ~/src/app -- "review PR 412; findings to /tmp/r412.md"
bowery done reviewer
bowery dispatch fixer --machine swarm-box --as alice --worktree fix-auth --cwd ~/src/app -- "..."
bowery orphans
```

## Joining Up

**Requirements:** Linux or macOS, [herdr](https://herdr.dev) (0.9.5 or later),
git. One static binary, nothing else to install.

```
curl -fsSL https://raw.githubusercontent.com/Techneosis/the-bowery/main/install.sh | bash
```

That fetches the latest release for your machine, checks it against the
release's SHA-256 sums, and installs it: the binary in `~/.local/bin` and tab
completion. To upgrade, `bowery upgrade` (or run the one-liner again);
`bowery upgrade --check` only asks, and exits 3 when there is a newer release.
Options go after `bash -s --`:

```
curl -fsSL …/install.sh | bash -s -- --claude          # also brief Claude Code, without asking
curl -fsSL …/install.sh | bash -s -- --version v0.1.0  # a particular release
curl -fsSL …/install.sh | bash -s -- --uninstall       # leave (records and config stay)
```

**From source** (needs Go): `git clone` this repository and run `./install.sh`
in it. It builds the binary and links everything into the checkout, so edits
are live.

If you use Claude Code, the installer also hands your agents the network's
standing orders. See [The Standing Orders](#the-standing-orders).

---

## How the Network Runs

**Sending someone out** (`bowery dispatch <name>`). One call makes the pane,
starts the agent, tags it quiet, hands it the job, and writes it down. A
courier whose start goes wrong is brought straight back, with whatever was put
out for them, so there are no half-dispatched agents loitering in panes.

**The Rooftop** (the `swarm` workspace). Couriers wait for work up top, out of
everyone's way: one workspace at the bottom of the list, one tab per agent,
labelled with its name. It is opened by the first dispatch and closed with
the last tab. Agents you want to watch can go beside you instead (`--split`,
`--tab`), or get a workspace of their own (`--workspace`).

**Their own route** (`--worktree <branch>`). An agent working on code gets a
fresh checkout on a new branch, cut from the remote's default branch as of a
fetch made that moment, never from a stale local main. It lives where herdr
keeps worktrees, `<worktrees dir>/<repo>/<branch>`.

**Travelling quiet.** Every courier carries a `sound=off` tag (a herdr pane
token) unless sent `--loud`, so a sound plugin of yours can stay silent for
them and ring only for the agents you talk to yourself.

**The book.** Every dispatch is written down (`~/.local/state/bowery/`): the
pane, the tab or workspace made for it, every worktree it works in. `done`
reads the book, which is how it knows what is the agent's and what is not.

**Bringing them home** (`bowery done <name>`). The agent's pane closes, and
with it the tab or workspace made for it and anything the agent opened there.
Its worktrees are removed and their branches deleted, *if* the checkout is
clean and the branch is merged (squash and rebase merges count). The main
checkout is fast-forwarded if it is clean. Every check runs before anything is
touched: a `done` that refuses has changed nothing, and says why.

**Back from the dead** (`bowery resume <name>`). A courier whose pane died,
whose herdr restarted, or who was sent home by mistake can be brought back
with their memory intact: the same name, the same route, the same box, the
same papers, picking up the conversation where it stopped. The first thing
they're told is that they were brought back, and to check what they already
did before doing it again. The book counts each return.

**Strays** (`bowery orphans`). Agents finished but still about, worktrees no
pane is in and no one's record claims, blank workspaces herdr left behind,
records whose agent is gone, and which of those `resume` can bring back.
Read-only: it tells you what `done` would clean, and that `done` ends any
chance of a return. `--json` for a program, which says when a box couldn't
be reached rather than calling it clean.

---

## The Rules

**1. Nobody is sent out unnamed.** A name is lowercase letters, digits, `-`
and `_`, and it is how the agent is addressed from then on.

**2. You only bring home your own.** `done` closes what was made for the
agent and nothing beside it. An agent split off next to your pane owns its
pane, not its neighbours. A tab that has been renamed is no longer taken for
the agent's. A worktree another pane is in, or that a live agent's record
claims, is never removed out from under it, whatever the flags say.

**3. No one is pulled off a job.** `done` refuses while the agent is working,
or blocked waiting on an answer, or while a pane in its tab is still running
something. `--force` overrides, and only those.

**4. Unfinished work is not thrown away.** A dirty checkout or an unmerged
branch stops `done`. `--force` removes the checkout but keeps an unmerged
branch; `--keep-worktree` leaves both.

**5. One wrong door doesn't bring the house down.** A herdr worktree group
closes all its workspaces when its primary's last pane is closed. The Bowery
never makes groups, and refuses any close that would cascade into one.

**6. Nobody comes back into someone else's room.** `resume` uses the
courier's own pane only if it is the same terminal, at a bare shell prompt;
a pane now holding another agent, or running anything, is refused. It never
starts a courier in a pane whose processes were started with another tool's
marker (`hands_off_env` in
the config). It refuses, changing nothing, while the courier or their
conversation is still about, when their worktree or folder is gone, when the
papers no longer say who they travel as, or when the room they'd come back to
is someone else's. A courier whose old room is still open comes back there,
not to a second one.

**7. Folders are trusted on your say-so only.** Claude asks before working in
a folder it hasn't seen. The Bowery answers yes under your trust roots (`~/dev`
by default, and the herdr worktrees dir) and nowhere else.

---

## Across the River

`--machine <label>` sends the agent to a saved herdr machine (`herdr machine
list`): a shared box running its own herdr server. Every herdr call goes
through `herdr --machine`, every git and file check through `ssh`. Paths are
the box's: `~/src/app` there, even when your shell expands the `~` here.

On a box shared by several people, each courier can travel **under a name**:
`--as alice`. The machine's `identity_command` (in the config) is a program on
the box that runs things as a person: their git author, their token. The
agent's shell becomes that person's before the agent starts, and the
credentials never leave the box. A machine with no `identity_command` refuses
`--as`, and its agents run as the ssh user.

The box itself can dispatch under a name too (a job there handing work to an
agent, say): a top-level `identity_command` in the box's own config allows
`--as` without `--machine`, with the same contract. Local git for that agent,
including `done`'s checks, runs through it.

```
bowery dispatch fixer --machine swarm-box --as alice --worktree fix-auth --cwd ~/src/app -- "..."
bowery done fixer                  # found by its record; no flag needed
bowery resume fixer                # likewise: back on the box, as alice
bowery orphans                     # this machine, and every box you have a record on
```

On a shared box only couriers you sent count as yours: `orphans` doesn't list
someone else's agents, and marks a worktree nobody here claims as "maybe
another dispatcher's".

---

## At the Desk

```
bowery dispatch <name> [placement] [options] [-- <prompt>]
    --swarm (default) | --split [right|down] | --tab | --workspace | --pane <ID>
    --from <ID>              the pane to split / whose workspace to use
    --cwd <DIR>              the new pane's directory (the repo, for --worktree)
    --worktree <BRANCH>      a new checkout on a new branch; --base <REF> to pick its start
    --also-worktree <PATH>   another existing worktree this agent works in (repeatable)
    --machine <LABEL>        on a saved herdr machine; --as <WHO> for an identity there
    --loud                   no sound=off tag
    --gated                  keep permission prompts on (default: bypassed, as an
                             unwatched pane would wedge on one)
    --kind <KIND>            agent kind (default claude); --model <MODEL>
    --launcher <CMD>         start claude through a wrapper that execs it (recorded; resume reuses it)
    --timeout <MS>           readiness timeout (default 60000)
    --wait                   block until the agent settles after its prompt

bowery done <name|pane|path>...   bring them home  [--force] [--keep-worktree] [--keep-branch] [--dry-run] [--machine L]
bowery resume <name> [--swarm|--workspace] [--gated|--ungated] [--launcher CMD|--no-launcher] [--if-dead] [--not-within DUR] [--dry-run] [-- <prompt>]   bring a dead one back
bowery adopt <name> <path>...     record more worktrees as the agent's
bowery orphans [--machine L] [--json]   what done would clean up
bowery list [--machine L]         agents, quiet or loud, who can be resumed, and how many strays
bowery mute|unmute <name|pane>... set or clear the sound=off tag
bowery swarm-tab <label> [--cwd DIR] [--machine L]   a rooftop tab with no agent; prints its pane
bowery swarm-last                 move the swarm workspace to the bottom
bowery config                     the settings in force
bowery upgrade [--check]          install the latest release
bowery version
```

`dispatch` prints one line of JSON: `{"name", "pane_id", "workspace_id",
"tab_id", "sound"}`, plus `"worktree"` and `"branch"` with `--worktree`, and
`"machine"` and `"as"` with `--machine`.

---

## The Arrangements

`~/.config/bowery/config.toml` (`BOWERY_CONFIG` names another). Every key is
optional; `bowery config` prints what is in force, and
[examples/config.toml](examples/config.toml) has them all.

```toml
swarm = "swarm"                 # the rooftop workspace's name
sound_source = "bowery.sound"   # the pane-metadata source of the sound=off token
trust_roots = ["~/dev"]         # folder-trust is answered under these (+ the worktrees dir)
hands_off_env = []              # env vars marking another tool's processes: resume never lands there

[models]                        # short names for --model (anything else passes through)
fast = "claude-haiku-4-5"

[machines.swarm-box]            # a saved herdr machine, by label
identity_command = "~/bin/as"   # `as <who> [argv]`; exports identity_env=<who>
identity_env = "BOWERY_AS"
# ssh = "swarm-box"             # default: the herdr profile's target
# trust_roots = ["~/dev"]       # on the box; default: the top-level list
```

The environment can point the network elsewhere, for tests and for running
several setups side by side: `BOWERY_STATE` (the records),
`BOWERY_WORKTREE_ROOT` (instead of herdr's worktrees dir), `BOWERY_TRUST_ROOTS`
(more trust roots, `:`-separated), `BOWERY_RELEASES` (where upgrades come
from).

**Hooking in.** The Bowery beeps nothing and orders nothing on its own; herdr
plugins of yours can. A sound plugin skips panes whose `sound` token is `off`;
a workspace-order plugin can keep `swarm` last as workspaces come and go.
`bowery swarm-tab` gives any other tool a pane on the rooftop. See
[docs/design.md](docs/design.md#integration-points).

---

## The Standing Orders

Most dispatches come from AI agents, so the network issues them standing
orders: a Claude Code skill (`agents/skills/bowery/`) that teaches an agent
when to send someone out instead of doing the work in-process, how to brief
them, and the rule that matters most: bring each one home with `done` as soon
as their result is checked, not in a sweep at the end.

When `~/.claude` exists, `install.sh` links the skill into its `skills/` and
offers to add a short notice to `CLAUDE.md` pointing agents to it. The notice
sits between marker comments, so a later install updates it in place and
`--uninstall` takes it out.

```
… | bash -s -- --claude          # add the CLAUDE.md notice without asking
… | bash -s -- --no-claude       # leave Claude Code alone
… | bash -s -- --claude-only     # only the skill and the notice
```

---

## The Premises

| | |
|---|---|
| `cmd/bowery/` | the whole network: one static Go binary |
| `completions/bowery.bash` | tab completion |
| `agents/` | the Standing Orders (a Claude Code skill) and the `CLAUDE.md` notice |
| `examples/config.toml` | every arrangement, with its default |
| `tests/` | rehearsals: `python3 -m unittest discover tests` (fake herdr, real git) |
| `tests/live/` | the same against a real herdr and real agents, by hand |
| `~/.local/state/bowery/` | the book |

How it works, plainly, and why: [docs/design.md](docs/design.md).

MIT licensed.

## Watching what's in flight

An orchestrator that has agents, PRs and CI runs in flight shouldn't poll
them in a loop. `bowery watch <config>` does it in the background and exits
as soon as something worth acting on changes, so the exit is the wake-up:

```
repo acme/web
machine swarm-box                  # agent lines below are on that machine
agent fixer                        # idle | done | blocked | gone (working is quiet)
pr 412                             # OPEN | MERGED | CLOSED, /CONFLICTING
run ci.yml feature-x               # latest run on a branch or commit (in progress is quiet)
ticket T-12                        # anything else: ~/.config/bowery/watch.d/ticket T-12
```

It prints `CHANGED:` with the lines that changed and `ALL:` with everything,
`STILL BLOCKED:` when an agent is at a dialog, or `HEARTBEAT` after `--max`
(45m) with nothing worth waking for. Every kind reports a coarse state, never
progress text, so it wakes on transitions rather than narration. A kind of
your own is a script in `watch.d`: its last line of output is the value, and a
leading `~` marks it quiet. `bowery watch --help` has the details.

