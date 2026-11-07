# The Bowery: how it works

*The courier network is in the [README](../README.md); this is the plain
version.*

bowery starts agents in [herdr](https://herdr.dev) panes, on this machine or on
a saved herdr machine, and tears them down when their work is collected. One
command does pane → agent → tag → prompt, records what it made, and `done`
uses that record to take back exactly that, and nothing that isn't the
agent's.

```
bowery dispatch reviewer --worktree review-412 --cwd ~/src/app -- "review PR 412; write findings to /tmp/r412.md"
bowery done reviewer
bowery dispatch worker --machine swarm-box --as alice --worktree fix-auth --cwd ~/src/app -- "..."
bowery orphans
```

## Why a wrapper at all

An orchestrating agent can drive herdr itself: split a pane, `agent start`,
`agent prompt`. In practice that leaves wreckage. A pane whose shell isn't up
yet refuses the start (`agent_pane_busy`), Claude's folder-trust dialog
blocks a fresh folder, a dispatched agent beeps at the person like any other,
and nobody remembers at the end which panes, tabs, worktrees and branches were
made for which agent. Teardown by hand is worse: herdr's worktree groups (see
below) mean closing the wrong pane takes a working sibling agent with it.

So the start is one call that cannot forget a step, and the record it writes
is what makes teardown safe.

## Dispatch

1. **Arguments are checked before anything is made**: names, placements that
   make no sense together, `--machine`/`--as` (below).
2. **The worktree**, with `--worktree <branch>`: a plain `git worktree add -b
   <branch>` at `<herdr worktrees dir>/<repo>/<branch with / as ->` (herdr's own
   layout), from the remote default branch freshly fetched (`origin/HEAD`, else
   `origin/main|master`), or `--base`. Never `herdr worktree create`: see the
   hazard.
3. **The pane**: a tab in the swarm workspace (default), a split, a tab beside
   you, a workspace, or a pane you name.
4. **The identity**, on a machine with one (below).
5. **`herdr agent start`**, retried while the fresh shell is not ready
   (`agent_pane_busy`, up to 15 s). If it fails `agent_not_ready` and the
   screen shows Claude's folder-trust dialog, it is answered Yes (`Down Enter`:
   the default option is "No, exit", so a bare Enter quits the agent), but
   only when the folder is under a trust root; anywhere else the start fails
   and says so. bowery does not decide to trust a folder the person hasn't.
6. **The tag** (`sound=off`, below), then **the prompt** if one was given.
7. **The record**, then the JSON line on stdout.

A start that fails at any step undoes what it made (the pane, through the
same group-safe close `done` uses; the worktree and its branch), so a failure
never leaves a half-started orphan.

Claude agents get `--dangerously-skip-permissions` unless `--gated`. A
dispatched pane is unwatched: a permission prompt there wedges the agent
silently until whoever waits on it times out. Other agent kinds get no flag
either way (the flag is claude's). `--model` is passed through as written, or
expanded through `[models]` in the config; bowery carries no table of model
ids of its own to go stale.

## Placement and ownership

| placement | the pane | what the agent owns |
|---|---|---|
| `--swarm` | a tab labelled `<name>` in the swarm workspace | that tab |
| `--tab` | a tab in your workspace | that tab |
| `--workspace` | a workspace labelled `<name>` | that workspace |
| `--split` | split off your pane | its pane only |
| `--pane <id>` | an existing pane | its pane only |

What bowery made for an agent is the agent's: whatever the agent opens there
later (a shell it split off, a dev server) is closed with it. A split or a
named pane sits among panes that are not the agent's, so there only its own
pane goes. The record says which (`owns: {kind, id}`); before closing an owned
tab or workspace, `done` checks its label is still the agent's name, so a tab
someone renamed, or an id herdr reused, is never taken for it.

**Swarm** is one plain workspace, rooted at `$HOME`, that holds every
dispatched agent as a tab: one sidebar entry however many agents run. It is
found or made under a file lock (two dispatches at once make one swarm),
closes with its last tab, and comes back on the next dispatch. After making
or finding it, bowery moves it to the bottom of the workspace list with
herdr's `workspace.move` socket method (the CLI doesn't expose it, so this is
local only). Keeping it at the bottom as other workspaces come and go is a
job for a herdr plugin, if you want it: herdr has no ordering setting.

## Done

`done <name|pane|worktree-path>` runs **every check before it changes
anything**, and is safe to re-run (a target with nothing behind it is a
no-op success).

It refuses when:

- the agent is `working` or `blocked` (`--force` overrides);
- a pane in the agent's own tab or workspace, other than the agent's, is not
  provably at an idle shell prompt: herdr's `pane process-info` must show the
  shell as the only foreground process (`--force` overrides);
- closing would cascade into a worktree group (below; never overridden);
- the caller's own pane would be closed;
- a worktree is dirty or its branch unmerged (`--force` removes the checkout
  but keeps an unmerged branch; `--keep-worktree` leaves both);
- a worktree is still in use by another pane, or claimed by another live
  agent's record (never overridden: `done <that agent>` is the way).

"Merged" means merged into the remote default branch, freshly fetched: an
ancestor, or a squash or rebase merge, detected as "merging it would change
nothing" (`git merge-tree --write-tree` gives the target's own tree).

Then: the owned tab or workspace closes (or the agent's pane), each worktree is
removed and its merged branch deleted, the blank repo-root workspace herdr may
have left is closed (only when idle and no worktree workspace of that repo is
still open), and the main checkout is fast-forwarded when it is clean and on
the default branch.

An agent whose pane was moved, or that exited back to a shell, is still
found: by `terminal_id`, which survives both where `pane_id` does not.

## Resume

`resume <name>` starts a dead agent again with its conversation: `claude
--resume <session>`, in the directory it ran in (claude files sessions per
directory), with the flags it had (`--gated`, `--model`), on its machine, as
its `--as`. The first prompt says it was resumed and to check what it already
did before redoing anything, since an agent may have placed paid or
irreversible work before it died; a prompt after `--` follows it. That is a
prompt, not a check: bowery can't know what an agent's work cost, so the
agent itself is asked to look.

The session id comes from herdr (`agent_session` in `herdr agent list`). It is
recorded at dispatch and refreshed whenever `list` or `watch` sees the live
agent in its own terminal: a `/clear` gives claude a new one. `orphans` and a
`resume` that hasn't started anything read the fresh id but write nothing. A
namesake in another terminal is not the agent and never overwrites it; an
older record without a terminal id is matched by pane, and with neither
nothing is taken. Each refresh re-reads the record just before writing and
gives way if it changed or went (a `done` in between wins).

Like `done`, it checks everything before it changes anything, records
included. It refuses:

- while the agent is alive, or any live agent carries its session (one
  conversation must not run twice);
- when the record has no session (written by an older bowery, never seen
  alive since) or no directory, or the agent is not claude;
- when its directory or any recorded worktree is gone (checked as its `--as`
  identity, whose directories may be closed to the ssh login);
- when its `--as` and the config disagree: dispatched as someone, but there is
  no `identity_command` now (it would start as the wrong person), or a box
  that has identities now and a record with none;
- for a record written before 0.3.0 (no `cwd` of its own), which doesn't say
  whether the agent was `--gated`: `--gated` or `--ungated` says, and is
  recorded;
- with `--swarm` or `--workspace` while its old pane, tab or workspace is
  still open, since a new one would leave the old owned by nothing;
- when the pane it would use is someone else's: the recorded terminal now
  holding another agent, or running anything rather than sitting at a shell
  prompt;
- when that pane's shell or foreground processes carry a variable named in
  `hands_off_env`: another tool's processes, read from `/proc` (or `ps -E`)
  where the pane runs, as its identity. That is the environment each process
  started with: a variable a shell exported later isn't seen. Only a reused
  pane is checked; a new pane is bowery's own fresh shell.

Where it lands: the agent's own terminal if it is still open at a shell (the
agent quit), moved back to its directory if need be; else a new pane in the
tab or workspace bowery made for it, if that is still open (checked by label,
as `done` does); else a new place of the kind it had: a swarm tab, or a
workspace for a `--workspace` agent. A `--split` or `--pane` agent never owned
the panes around it, so once its own pane is gone it comes back in a swarm
tab rather than beside a neighbour. `--swarm` and `--workspace` override
(refused while the old place is open, above). The record's pane, terminal,
tab and workspace ids are rewritten every time, and for a new swarm tab or
workspace its `placement` and `owns` too, so `done` brings it home as usual;
`resumed` and `resumes` say when and how often.

The record is written as soon as the agent is up, before its sound tag and
the notice, so a failure after that leaves a record that points at the
running agent (and says so) rather than at the pane it left. A start that
fails takes back what it made, and a reused pane's shell is moved back to
where it was.

Only a record makes an agent resumable: an agent bowery didn't dispatch (a
pane another tool started) has none, and is out of reach.

## Records

One JSON file per agent per server, in `$XDG_STATE_HOME/bowery`
(`~/.local/state/bowery`), named `<name>.<8 hex>.json`. The hex is the first
eight of the SHA-1 of the server key: the local herdr socket path, or
`machine:<label>`. So an agent name is per server: a throwaway test session
never reads the real session's records, and a machine's records are filed
apart from the local ones.

```json
{
  "name": "reviewer", "socket": "/home/you/.config/herdr/herdr.sock",
  "placement": "swarm", "kind": "claude", "started": "2026-11-01T09:00:00+00:00",
  "worktree": "...", "branch": "review-412", "repo": "...", "base": "origin/main",
  "pane_id": "w3:p1", "terminal_id": "term_...", "workspace_id": "w3", "tab_id": "w3:t2",
  "created_pane": true, "sound": "off",
  "worktrees": [{"path": "...", "repo": "...", "branch": "review-412"}],
  "owns": {"kind": "tab", "id": "w3:t2"}
}
```

A remote record has `"socket": null` and adds `machine`, `host` and `as`.
For `resume`: `session`, `cwd`, `gated`, `model`, and after a resume,
`resumed` and `resumes`. Each is optional, so a record without them still
loads (and is not resumable without a session).
`worktrees` is every checkout the agent works in: its own, `--also-worktree`,
and `adopt`. Records written before `owns` existed are read by their
placement. The format is the one the Python tool bowery was ported from
wrote, file names included, so its records can be copied into the state
directory and are read as they are.

## Orphans

`orphans` lists what `done` would clean up, read-only (it writes no file):
dispatched agents that
are finished but alive; checkouts under the worktrees dir that no pane is in
and no live agent's record claims (with their state: dirty, merged,
unmerged); blank repo-root workspaces; and records whose agent is gone (with
its pane, tab or workspace still open, its worktree still there, or nothing
left at all), each marked `(bowery resume <name>; done ends that)` when resume
can bring it back. A dead agent whose own pane is still open at a shell is
listed only then. A resumable agent's worktree line says so too, since `done
<path>` ends the resume as surely as `done <name>`. One record whose check
fails (an ssh hiccup) is warned about and taken as not resumable; it doesn't
lose the rest of the box.
`--json` prints the same as a list of objects (`kind`, `target`, `name`,
`machine`, `status`, `repo`, `branch`, `merged`, `state`, `note`,
`resumable`, `resumes`, ...) for a program, with an entry of kind
`unreachable` for a machine that couldn't be read, so a program never takes
it for a clean one. Locally, an
agent tagged `sound=off` counts as dispatched; on a machine only a record made
here does, because a shared box runs other people's dispatches too.

It covers the local server and every machine there is a record for; a machine
that cannot be reached is reported and skipped, never fatal.

## Remote dispatch

`--machine <label>` names a saved herdr machine (`herdr machine list`). Every
herdr call goes through `herdr --machine <label>`, which forwards it to that
box's server. Git and file checks go through `ssh` to the profile's target (or
the machine's `ssh =` in the config), with BatchMode and a shared control
connection. ssh failing (255) or a missing tool on the box (126, 127) is an
error, never an answer: otherwise "not a worktree" or "not merged" could be
read from a dead connection, and done would act on it.

Paths given with `--machine` are paths on the box: `~/...`, and this
machine's own `$HOME/...` (a shell expands `~` before bowery sees it), map to
the box's home, read over ssh along with the box's herdr worktrees dir. The
repo must already be cloned there; bowery never clones, installs or starts
anything on a box.

**Identities.** A shared box has one Unix user and several people. A machine
can name an `identity_command` in the config: a program on the box such that

- `<cmd> <who> <argv...>` runs argv as `<who>` (their git author and
  committer, their forge token, whatever the identity holds),
- `<cmd> <who>` with no argv is their interactive shell,
- both export `identity_env` (default `BOWERY_AS`) set to `<who>`,
- and, optionally, `<cmd> --list` prints who has an identity.

With one, `--as <who>` is required: bowery checks `<cmd> <who> true` works on
the box before making anything, then runs `exec <cmd> <who>` in the new
pane, proves the switch by reading `$<identity_env>` back from the pane, and
only then starts the agent, which inherits that shell's environment. The
credentials never leave the box; they never pass through this machine's argv
either. Remote git (fetch, worktree add, done's checks) runs through the same
command. A machine without an `identity_command` refuses `--as`, and its
agents run as the ssh user.

The command is written into a remote shell line unquoted, so `~` in it works;
a non-login ssh session has a short PATH, so give a full path.

`--split`, `--tab` and `--from` are refused with `--machine`: they mean
"beside your pane", which is local.

## The worktree-group hazard

`herdr worktree create` groups the new workspace under a primary workspace on
the repo's main checkout: a blank one labelled with the repo name if none is
open, or one you name. `herdr pane close` on the primary's last pane closes
the whole group, killing every agent in a sibling worktree. `herdr workspace
close` on such a primary refuses (`workspace_group_close_required`). Measured
on herdr 0.9.5.

So bowery never makes groups (`--worktree` is a plain `git worktree add`, and
swarm is rooted at `$HOME`, not in a repo), and copes with groups made by
hand: a last pane is closed with `workspace close`, which refuses to cascade,
and a group primary with open siblings is refused outright.

## Sound

The tag is a herdr pane token, `sound=off`, reported with `pane
report-metadata --source <sound_source>` (default `bowery.sound`). Tokens live
until the pane closes and show in `herdr agent list`. bowery does not beep or
silence anything itself: the point is for a sound plugin of your own (with
herdr's built-in sound off) to skip panes carrying the token, so only the
agents you talk to directly are heard. A remote pane gets the token too
(herdr forwards it); whether anything on the box listens is up to the box.

## Integration points

- **A sound plugin** reads the `sound` token from `herdr agent list` (or the
  pane metadata events) and stays quiet for `off`. `sound_source` names the
  source bowery reports under, if your plugin filters on it.
- **A workspace-order plugin** can keep the swarm workspace (`swarm` in the
  config) last as workspaces come and go; `bowery swarm-last` does the same
  move once.
- **`bowery swarm-tab <label> [--cwd DIR]`** gives another tool a tab where
  dispatched agents live, without an agent or a record, and prints its pane
  id: for watchers and test harnesses that want their panes out of the way.

## Testing

The suite is Python and black-box: it drives the binary (`BOWERY_BIN`, or a
fresh build) through its CLI, with a fake `herdr` and a fake `ssh` first on
PATH (`tests/fakes/`) and real git. The fake herdr keeps each server's state
in a JSON file and answers in the real CLI's shapes; it models the behaviour
bowery depends on (the not-ready shell, the trust dialog, group cascades,
process-info), and the box is a directory the fake ssh runs commands in. So
the remote paths are tested without a second machine.

`tests/live/dispatch.sh` runs the same story against a real throwaway herdr
server with real claude agents on haiku, by hand: it is how the fake is held
to the real thing.
