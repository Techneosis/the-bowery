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
6. **The tag** (`sound=off`, below), then **the prompt** if one was given,
   and **the start check**: herdr's `agent prompt` can return with the text
   typed into the composer and never submitted, and an agent sitting idle
   says nothing. So dispatch waits up to 60s for the agent to leave idle
   (working, blocked or done), sends one more Enter if it hasn't, and gives
   it 15s more. Still idle, it exits 1 and says so, and leaves the agent up:
   the prompt is in its composer, and tearing it down would lose the one
   thing a person needs to see. `--wait` skips it (herdr already saw it
   settle), and so does `--no-start-check`.
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

`--launcher <CMD>` starts claude through a command of the user's: a wrapper
that sets up the agent's environment and then execs claude. `herdr agent
start` only runs an agent kind's own executable, so with a launcher bowery
runs `BOWERY_LAUNCH=1 <CMD> <claude's arguments>` in the pane's shell
itself (the variable tells a launcher that bowery sends the first prompt,
so one with an opening prompt of its own can leave it out), waits for herdr
to see a claude agent in that pane, names it with `herdr agent rename`,
answers the folder-trust dialog as a plain start does, and carries on (tag,
record, prompt). A launcher that doesn't exec claude fails the start, with the
pane's last lines, and what was made is taken back (in a pane that wasn't
bowery's, it says the line ran there and leaves it). On a resume, a launcher
that starts claude on another session than the one asked for (one that
drops claude's arguments, say) is refused before anything is recorded. The
line is typed into a shell, so it must be one plain line: not empty, no
control or format characters, and not an option taken by mistake
(`--launcher=VALUE` for a command that really starts with `-`). For a launcher start,
`--timeout` is the whole start's total (whole milliseconds, at most a day:
past that a deadline wraps into the past). A resume keeps the record's session
unless herdr confirms the same one, and says so when it can't. The launcher is recorded,
so `resume` starts the agent through it again; `resume --launcher CMD` sets
one and `--no-launcher` drops it.

## Placement and ownership

| placement | the pane | what the agent owns |
|---|---|---|
| `--swarm` | a tab labelled `<name>` in the swarm workspace | that tab |
| `--tab` | a tab in your workspace | that tab |
| `--workspace` | a workspace labelled `<name>` | that workspace |
| `--split` | split off your pane | its pane only |
| `--pane <id>` | an existing pane | its pane only |

`--space <label>` changes one thing for one call: the workspace a swarm tab
goes in, found by that label (letters, digits, `.`, `_`, `-`, up to 48: no
spaces, so it is one argv word everywhere it is passed) (never a worktree group's) or made rooted at
`$HOME` like swarm. It isn't recorded. The record's `owns` names the tab, and
that's all `done` needs. Only the configured swarm is moved to the bottom of
the list. A workspace someone else keeps for its own purpose stays where they
put it. It's refused with any placement but `--swarm`, since only a swarm tab
has a workspace to choose.

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
- when its recorded launcher isn't one plain command line (`--launcher CMD`
  or `--no-launcher` overrides it), checked before `--not-within` so a "too
  soon" can't hide it;
- when its directory or any recorded worktree is gone (checked as its `--as`
  identity, whose directories may be closed to the ssh login);
- when its `--as` and the config disagree: dispatched as someone, but there is
  no `identity_command` now (it would start as the wrong person), or a box
  that has identities now and a record with none;
- for a record written before 0.3.0 (no `cwd` of its own), which doesn't say
  whether the agent was `--gated`: `--gated` or `--ungated` says, and is
  recorded (on a newer record, `--ungated` over `--gated` is for that start
  only: the record stays gated);
- with `--swarm` or `--workspace` while its old pane, tab or workspace is
  still open, since a new one would leave the old owned by nothing;
- when the pane it would use is someone else's: the recorded terminal now
  holding another agent, or running anything rather than sitting at a shell
  prompt;
- when that pane's shell or foreground processes carry a variable named in
  `hands_off_env`: another tool's processes, read from `/proc` (or `ps -E`)
  where the pane runs, as its identity. A process whose environment can't be
  read is a refusal too: a hands-off rule fails closed. Without `/proc` (a
  Mac), the environment is only what `ps -E` adds after the plain command, so
  a marker in a process's arguments is not taken for one; a process `ps -E`
  shows no environment for (another user's, or a system binary's) is refused. That is the
  environment each process started with: a variable a shell exported later isn't seen. Only a reused
  pane is checked; a new pane is bowery's own fresh shell.

Where it lands: the agent's own terminal if it is still open at a shell (the
agent quit), moved back to its directory if need be; else a new pane in the
tab or workspace bowery made for it, if that is still open (checked by label,
as `done` does); else a new place of the kind it had: a swarm tab, or a
workspace for a `--workspace` agent. A `--split` or `--pane` agent never owned
the panes around it, so once its own pane is gone it comes back in a swarm
tab rather than beside a neighbour. `--space` names the workspace for that new
tab, and for a `--workspace` agent asks for a tab there instead of a workspace;
it never skips the agent's own pane or tab. `--swarm` and `--workspace` override
(refused while the old place is open, above). The record's pane, terminal,
tab and workspace ids are rewritten every time, and for a new swarm tab or
workspace its `placement` and `owns` too, so `done` brings it home as usual;
`resumed` and `resumes` say when and how often.

The record is written as soon as the agent is up, before its sound tag and
the notice, so a failure after that leaves a record that points at the
running agent (and says so) rather than at the pane it left. A start that
fails takes back what it made. A reused pane back at a bare shell is moved
back to where it was; one with something left running in it is never typed
into: closed if bowery made it, else left with a warning.

**Unattended** (a mail hook, a job): `--if-dead` makes "it's alive" and
"another resume of it is running" a quiet exit 0 rather than a refusal,
so a caller needs no checks of its own; every other refusal still fails.
`--not-within DUR` refuses an agent resumed that recently, so a seat that
dies on start can't be restarted every minute. It's the last check, so
under `--if-dead` "too soon" never hides a refusal a person needs to see;
a `resumed` time bowery can't read is a refusal, and one in the future
counts as just now. `resume` and `dispatch` each hold a lock on the agent's
name (per name and server, taken without waiting), so a hook's resume can't
race another resume or a dispatch of the same agent: `resume` takes it once
the record is known; `dispatch` takes it for the new name and lets go once
the record is written, before its prompt, so `--wait` doesn't hold it.
`done` takes it too, for every agent its target reaches: the record a name
or pane resolves to (a pane by its terminal, as `done` itself finds it), the
live agent's name, and for a worktree path every record that claims it,
since `done` rewrites each. It waits up to two seconds for each, then
refuses, and resolves the target again once they're held: if it reaches a
different agent by then, it refuses. A name with no record has its lock
taken only if the lock file is already there (a dispatch of it may be
mid-way), so `done <typo>` leaves nothing behind. `list` and `watch` take
it, without waiting, to write a session they've seen change, and skip a
record someone holds, so a `done` can't drop a record they then put back.
`dispatch` of a name that already has a record is refused: replacing it
would lose the agent's session and resume count (`done <name>` clears it).
`done <worktree path>` strikes the worktree from every record that claims
it, and drops a record whose own directory was in it (there is nothing left
to resume), unless its agent is alive, or herdr can't say whether it is (then
the record is only struck). Once its record is written, a
failed sound tag or prompt says the agent is up and recorded, and prints
its JSON line. The lock files live in the state dir's `locks/`
folder and stay there: removing one would race the next taker.

`--fresh` starts the agent again in a **new** conversation instead, with
the same checks, lock, landing and launcher, and a notice that says its
earlier conversation isn't loaded. It needs no session on the record (but is
refused while the old session is live somewhere), and the record takes the
new session, once herdr confirms it, so the next plain resume continues the
fresh one. It counts as a resume, so `--not-within` still guards it.

`--json` makes `resume` print exactly one JSON line on stdout, whatever
happens (a usage error and a crash included), with an `outcome`: `resumed` or `started_fresh` (with the pane, tab
and session, as before, and `gated`: how this start ran, which after
`--ungated` on a gated record differs from what the record keeps, and then
`"ungated_once": true` too), `alive`, `busy` (another resume, dispatch, adopt or
done holds the name), `too_soon`, `dry_run`, `refused` or `failed` (with `why`). Under
`--if-dead` the first five exit 0 and the last two don't, so a caller can
tell "it's up" from "too soon" without reading prose. The record is written
as soon as the agent is up, so a crash after that leaves a recorded, live
seat (which a caller sees as `alive`), and the old record is kept to put
back. Within one 10-second deadline, bowery then checks which conversation
came up: the record's own on a resume, a new one on `--fresh`. A different
one (a launcher that drops its arguments, or passes `--resume` itself) takes
the start back. None named is `"warning": "session_unconfirmed"`, and the
record keeps its session until a `list` or `watch` sees one. Then the first
prompt (the notice and the caller's reason): one that never arrives takes
the start back too, since a seat up with no reason would just sit idle. A
take-back puts the old record back, reports `failed`, and says only what it
knows of what's left (taken back; or still running, unnamed, in the agent's
own pane, which a later resume then names). A record that can't be written
is `"warning": "record_not_written"` on a started agent, never `failed`, and
a sound tag that didn't take is `"warning": "sound_not_set"` (the `sound` field
is then what was asked for, not what the pane has). A record with no session (an older one, or a fresh
start herdr didn't name) is offered by `list` and `orphans` as
`bowery resume <name> --fresh`, and with `--gated|--ungated` when it's too old
to say how it was started.

**A record for an agent started by hand:** `adopt <name> --launcher CMD
--ungated` (or `--gated`) gives a live agent with no record one that `resume`
can use: its session from herdr, the folder its claude runs in (the pane's
foreground folder, not where the shell has moved to), and the launcher and
permission mode given. One of `--gated`/`--ungated` is required for a new
record, since bowery can't see how a hand-started claude was launched. On an
existing record `--launcher` sets the launcher, and `--gated` sets the mode;
`--ungated` over a record that says `--gated` is refused, since adopt never
widens a seat (`resume --ungated` does, for one start). An existing record
that lacks a session or a folder of its own (one from before 0.3.0, or a
`--fresh` start herdr never named) is filled from its live agent, the
session and the folder both from that one, and only when the agent sits in
the record's own terminal (or, for a record with none, its own pane): a name
alone can't say which conversation is its. A record from before 0.3.0
doesn't say how it was started, so it needs `--gated` or `--ungated`.
Everything is checked
before the record is written: no session reported, two live agents of the
name, a bad name or path are refused with nothing written or said. Nothing
restarts. Paths starting with `-` go after `--`.

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
`resumed` and `resumes`. `ticket` is `--ticket`'s id; a resume rewrites the
record it read, so the ticket stays. Each is optional, so a record without them still
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

## List

`list` prints a line per live agent (pane, sound, status, name, title) and
one per dead record that `resume` can bring back, then an orphan count on
stderr. `--json` is the same servers as one array for a program: every live
agent (dispatched or not: `dispatched` says) and every record whose agent is
gone, resumable or not, each with the same keys, null where there is no
value. `state` is herdr's status for a live agent and `gone` for a record.
`last_seen` is the time of the call for a live agent and null for a gone one:
bowery keeps no clock on agents, so it doesn't know when one went. A machine
that can't be read is an entry of its own, `state: "unreachable"`, as in
`orphans --json`. It skips the orphan scan, the slow part of `list`. Like the
text, it writes fresh session ids into the records it reads; nothing else.

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
pane, proves the switch by reading `$<identity_env>` back from the pane,
`cd`s back to the agent's folder and checks it got there (a `sudo -i` style
command starts in the identity's home, and claude files a session under the
folder it starts in), and only then starts the agent, which inherits that
shell's environment. A pane bowery was handed (`--pane`) isn't moved: its
agent starts, and is recorded, wherever the shell is after the switch.

A resume in the agent's own terminal runs no `exec`: that terminal's shell
is the one `exec` replaced at dispatch, so it is still `<who>`'s, and the
identity may not be allowed to run the command itself (a `sudo -u`
wrapper). That rests on the terminal, not on the variable: reading
`$<identity_env>` back there only refuses a shell that has plainly lost it.

`--split` and `--tab` are opened with `--cwd`, the folder recorded for the
agent. Without it herdr opens them in the source pane's folder, which isn't
the recorded one when bowery runs from elsewhere (`--from`). The
credentials never leave the box; they never pass through this machine's argv
either. Remote git (fetch, worktree add, done's checks) runs through the same
command. A machine without an `identity_command` refuses `--as`, and its
agents run as the ssh user.

The command is written into a remote shell line unquoted, so `~` in it works;
a non-login ssh session has a short PATH, so give a full path.

`--split`, `--tab` and `--from` are refused with `--machine`: they mean
"beside your pane", which is local.

## Watch

`bowery watch <config>` is for an orchestrator with several things in
flight: instead of polling them itself, it starts the watch in the
background and is woken by its exit. Each config line is `<kind> <key>`;
`machine <label>` and `repo <owner/name>` set the machine and repo for the
lines after them. Built-in kinds: `agent` (herdr's status, one `agent list`
per machine per poll; `gone` when absent, `?` when herdr can't be read),
`pr` (`gh pr view`: state, plus `/CONFLICTING` when open and conflicting;
`UNKNOWN` mergeability after a push is ignored), `run` (the latest run of a
workflow on a branch, or on a commit for a hex ref of 7 or more). Any other
kind runs `~/.config/bowery/watch.d/<kind> <key...>` with
`BOWERY_WATCH_MACHINE` and `BOWERY_WATCH_REPO`; its last line of stdout is
the value (a tool's notice can come first), a leading `~` marks it quiet,
and a non-zero exit or empty output is `?`. A missing script is
`unknown-kind`.

Every value is coarse (a state, never progress text), so the watch wakes on
transitions, not narration. A line wakes it when its value differs from the
last one seen, unless the new value is quiet (working, queued, in progress)
or `?`, which is a failed read and never a change. The last values are kept
in `<config>.state` (a `?` is never written), so a re-armed watch compares
against what the last one saw; a line not seen before, or a first run with
no state, records without waking. One exception: on its first poll, an
agent already `blocked` exits at once (`STILL BLOCKED:`), since a dialog
nobody answers is the costliest thing to sleep through. Otherwise it exits
with `CHANGED:` and `ALL:`, or `HEARTBEAT` after `--max`, so a watch that
died can't pass for a quiet one. Polling `agent` lines also refreshes the
records' session ids, like `list`.

## Placement into offices

An **office** is one Unix user on one machine, with a root-owned record of
what it holds and who may place work in it, and rules that decide when it is
open. A job runs with the office's privilege and nothing more. The office
host is a second binary, `office`; bowery's part is the resolver.

### Files

| file | where (root mode) | holds |
|---|---|---|
| region file | `/usr/local/etc/offices/region.toml` | the region's name, timezone, public key, offices, priorities, reserved effects, tiers, naming, defaults (rule timeout, retry tick), region-wide rule instances, and its peers (address, pinned key, `may_place`, peer rules) |
| machine file | `/usr/local/etc/offices/machine.toml` | the machine's name, the placement transport, the region's private key for publishing (`region_key`), installed rule plugins (pinned by sha256), surfaces, machine-wide rule instances |
| office record | `/usr/local/etc/offices/<office>/office.toml` + `repos`, `signers`, `authors` | identity, kinds, profile, surfaces, holdings, signers, `[rules]` |
| office state | `/var/lib/offices/<office>/state` | the owner's override: open or closed, optionally `until`, `kinds`, `scope` |
| office log | `/var/lib/offices/<office>/log` | every flip, verdict, acceptance, refusal and launch, as JSON lines |
| `[placement]` | bowery's `config.toml` | which regions and offices to consider, ranking, per-kind defaults, the askers' signing keys (`keys`) and the region's (`region_key`) |

Every config decoder refuses unknown keys. In root mode every config file and
plugin must be owned by root and writable by nobody else, and `office pull`
must run as the office's user. `OFFICE_ROOT=<dir>` is dev mode: the same
files under `<dir>/etc` and `<dir>/var`, run as you, with exactly those three
identity checks relaxed, and a line on every command that says so. Rules,
placements, queues, verdicts and logs are the same code in both. The machine
file's name must match the host in both, so a copied config refuses to act
for another machine.

### Rules

A rule instance names a plugin and gives it config. Instances apply from the
region's `[[defaults.rules]]`, the machine file's `[[rules]]` and the office's
`[[rules.use]]`, and run in that order, concurrently, each under its own
timeout. An `exec` plugin is one process per evaluation: the request (contract
1) as JSON on stdin, an environment with nothing in it but its private state
directory (`OFFICE_RULE_STATE`), working directory `/`, stdout capped at
64 KiB, its process group killed at the deadline. Its description (`{"contract":
1, "describe": true}`) gives its phases, contracts, history and config schema,
and every evaluation checks the instance against it (and the config against the
schema) before running it.

The answer is decoded strictly. A plugin that doesn't match its pinned sha256,
doesn't speak the instance's contract, times out, exits non-zero, crashes, or
answers something that doesn't decode (an unknown field, `closed` with no
reason, a time that isn't RFC 3339) has **failed**, and a failed rule is
`closed` with the reason `rule <name> failed: <what>`, logged `rule-failed`.

Combining: all must pass, defer drops out, and with no opinion at all the
office's `default` decides. The reason shown is the first `closed`; the retry
time is the latest among them, or none if any closed verdict has none. Then
the owner's state: `close` beats every rule, `open` every rule but a `binding`
one. `office clear` drops the override. Each instance's verdict is one log
line (instance, plugin, its version and sha256, contract, verdict, reason,
retry, note, ms, and the combined result), and the evaluation one more.

`hours` (`cmd/rules/hours`) is the reference plugin: weekly windows in the
office's timezone. Its verdict doesn't depend on the job, so it is office-wide
(`holds: "office"`) until the moment it would change, and `describe`
publishes it until then.

### The resolver

`bowery place` (or `dispatch` with a job flag) builds a job, refuses a
reserved effect (exit 3), and goes over the region's offices: excluded by
`consider`, a pin or `never`; passed over if it is one of this region's
offices on another machine (of this region, only this machine's offices are
reachable yet; a peer's are, through its catalog) or if `office describe`
doesn't answer;
filtered on what each office holds (the same `Mismatch` function the office
uses when it accepts); then a published office-wide `closed`, else a probe
(`office describe --probe`, the job on stdin), which the office answers by
running its own place-phase rules. Survivors are ranked (`prefer`,
`holds-repo`, `same-machine`, `same-region`, `fewest-running`, then region and
name). The top one gets the placement, through the transport, and bowery exits
0. If none passes but some office was only closed (or didn't answer), the job
waits in `~/.local/state/bowery/queue/<id>.json` with the whole filter table,
the asker and the best closed office's owner are told once, and bowery exits
4. If no office could take it even open, that is a setup gap: exit 1 and
`unplaceable`, not a wait. `place --retry` retries the queue in priority
order, oldest first, drops the expired, and prints `next_retry` for a
scheduler. Each decision is a line in `~/.local/state/bowery/place.log`.

In `dispatch`, `--kind` stays the agent kind it always was, so a job's kind
there is `--job-kind`; `--office`, `--scope`, `--needs`, `--asked-by`,
`--priority`, `--pin`, `--tag` and `--surface` also make it a placement. A
placed dispatch writes no herdr record: the office's surface starts the agent.

### The placement and the pull job

A placement is one fenced block of `key = "value"` lines, sorted: the bytes
the signatures cover. It is bound to one office (`office`, `machine`,
`region`), names the asker's region (`from`), and its id is the job's key.
The letter is that block followed by the asker's signature and, from another
region, that region's seal (see "Regions and peering"). The transport is one
interface (send to a box at an address, wait on a box, reply to an asker at
an address, publish a catalog to a peer, read the catalogs peers sent),
picked by name in the machine file and bowery's config; the built-in one,
`dir`, makes each box a directory under `var/boxes/` and takes another
region's var directory as an address; `http` (below) takes a URL. Nothing
else knows how letters travel.
An empty address is this region's own, so a peer with none is never sent
to, published to or replied to: the resolver marks it unreachable, `office
publish` reports it, and a reply is logged `reply-failed`. A box or catalog
directory is read only for regular files of at most 4 MiB, opened without
following a symlink and without blocking, so a FIFO, a symlink or a huge
file there is passed over, not read. When the pull job files a letter
(accepted, refused, dropped) it writes the bytes it verified, atomically,
never whatever is at the path by then.

`office pull <office>` is the office's pull job. For each letter, in priority
order: an unreadable one, or one not signed by a key its signers file lists,
is dropped (and answered to nobody); a peer's that can't come in is refused
(see below); one for another
office, of a kind it doesn't take, or needing something it doesn't hold, is
refused (logged, one reply to the asker); a second copy of an accepted id is
discarded; an expired one is dropped and the asker told. The rest are judged
by the accept-phase rules and the owner's state: closed stays in the box
(`place-waiting`, logged once per reason), open is accepted and started
through the office's surface (a command from the machine file, given the job
as JSON). `--loop` is the long-running form: it waits on the inbox through the
transport's `Wait`, which returns on new mail or at the backstop, checks the
owner's state and the config every `--interval`, and judges a waiting
placement again only when its retry time comes, an event happens, or the
region's `retry_tick` passes. A transport that can long-poll implements
`Wait` by blocking, and the loop doesn't change.

Every stage is timed: the placement carries `resolve_at` and `signed_at`
(the asker's clock), the transport gives the send time (a file's mtime for
`dir`; a signed block can't carry a time that comes after it is signed), and
the office logs `delivered`, `verified` (its signatures checked), `accepted`
and `started` with all the earlier ones (`stages` on the `launched` line).
`place.log` has the resolver's half: `resolve`, `resolved`, `sign`, `seal`
(between regions), `send` and `sent`. Across a peer hop the two halves join
on the job id into one timeline.

### Rules on the machine

**State-file rules.** A machine-file plugin of kind `state-file` names a
JSON file another service writes (`path`), how old it may be (`max_age`,
required), and which fields mean what: `open_field` (a boolean),
`reason_field`, `retry_after_field` (RFC 3339 or Unix seconds), `at_field`
(the file's own timestamp; optional) and `facts` (fields exported as facts).
No plugin code runs: the core reads the file. It fails closed: missing,
unreadable, not a regular file, malformed, a wrong type in any named field,
or older than `max_age` by its mtime or its own timestamp, whichever is
older. In root mode the file and every directory above it must be root's and
writable by nobody else, and a failure of that is an integrity failure (see
below). Its verdict is the same for every job, so `describe` publishes it,
read live each time (a gate flips every few seconds; a cached verdict would
keep a resolver passing over an office that has opened), valid until the
file would go stale. Its instances take no config. The office log carries
the file's mtime where an exec rule's line carries its sha256.

**Facts.** `[facts.<name>]` in the machine file is a provider: a pinned
executable run like a plugin (empty environment, `/`, capped, killed at the
rule timeout) that prints one JSON object of the facts it declares, at most
every `interval`. The pull loop keeps readings for its lifetime; a one-shot
command reads fresh. A rule receives only the facts its description
declares, as `{value, at}`; a fact that is missing, whose provider failed,
or that is older than twice its interval (or, from a state file, past
`max_age`) is left out. A provider that fails is logged `fact-failed` with
smoke. Two sources for one fact name is a config error.

**Running work and the run phase.** A job whose surface started it (exit 0)
gets a running record (`var/<office>/running/<id>.json`); it ends when the
job reports `office done <office> <id>` or the owner stops it. Each pass of
the pull job runs the run-phase rules (component `run`) for every running
job before it takes in new post, every `rules.interval` (office, then
region, default 10s) under `--loop`, and on any event. A closed result asks
the job to do what the closing source's `on_closed` says: the instance's,
else the office's, else the region's, else `yield`; for the owner's close
it's the office's. The office host runs the surface's `hold` command with
`OFFICE_ON_CLOSED` and `OFFICE_REASON`; `yield` and `finish-turn` leave the
job `held`, `stop-next-turn` leaves it `stopping` (it ends when it reports
done). `office yielded` is the job saying it reached its safe point. A held
job is never re-placed. When its run-phase result is open again, the resume
is re-checked (the binding, the holdings, the accept-phase rules; not
expiry, which bounds starting, not running) and the surface's `resume`
command runs. While any job is held, new placements wait ("holding …"). A
surface with no `hold` command still has its jobs marked held, with smoke.

`office close --now` marks the state; the pull job then holds every
running job with `OFFICE_ON_CLOSED=now`, logged as the owner's stop.
`office stop <office> <job>` leaves a request; the pull job runs the
surface's `stop` and kills the job's process group. Owner and agent never
talk directly: only the pull job, as the office, touches the office's
agents.

Repeated identical run-phase verdicts for one job and instance collapse:
the first is logged when it comes; the repeats become one line with
`count`, `first` and `last` when the verdict changes or the job ends.

**Owner overrides, logged.** Each flip records what it replaced (`was`). An
evaluation the owner's state decided carries it (`override`). An `until`
that runs out is logged once (`state-expired`), and the loop wakes for it.

**Integrity beats the owner.** A plugin whose sha256 doesn't match its pin,
one with no pin, or a plugin or gate that isn't root's alone in root mode,
closes the office even under the owner's `open`: the owner vouched for the
rules, not for whatever is now at that path. A plain failure (a crash, a
timeout, a bad answer) of a non-binding rule still yields to `open`.
Plugins and providers run from the opened, hashed file (as
`/proc/self/fd/3` on Linux), never re-opened by path; in root mode every
directory above a config file, plugin or gate, up to `/`, must be root's
and not writable by group or others.

**Who can write what.** The owner's state is at `var/owner/<office>/state`
and the decisions log (verdicts, evaluations, flips, expiries, stop
requests) at `var/decisions/<office>.log`, both outside the office's own
directory. In root mode the state, and every directory above it up to the
var root, must be out of the office user's reach, or it's refused (closed);
the office's own user can't open or close it. Decisions are written through
the recorder: `sudo -n -u <recorder> office record <office>`, the machine
file's `recorder` user, which takes decision lines only, so the pull job can
add to the record but can't rewrite or remove it. The activity log
(`var/<office>/log`) stays the office's. `office log` reads both in time
order.

**Surfaces.** Every surface command runs in its own process group, with
the surface's `timeout` (default a minute); at the timeout the whole group
is killed. In root mode its environment is built from nothing: a fixed
`PATH`, the office user's home, the job's names, never the caller's
`OFFICE_ROOT` or anything else.

**The long-poll.** `office pull --loop` blocks in the transport's
`Inbox.Wait` until mail lands (both built-in transports wait on a watch of
the box's directory: inotify on Linux, kqueue on macOS; they poll every
`--interval` elsewhere), while
checking the event fingerprint (the owner's state, the config files, the
gate files, running work and requests) every `--interval`. It wakes for
whichever comes first, or for a waiting placement's retry, an owner's
`until`, or the run interval, and never sleeps past `--backstop`.

**The http transport.** For regions on different machines. Each machine runs
`office serve`, bound to the machine file's `listen` (`host:port`: loopback
or a private interface, with the network's own front before it, such as a
tailnet's serve or a reverse proxy; it never runs as root). It serves three
paths and nothing else, and never lists anything:

| request | what it does |
|---|---|
| `POST /placements/<box>` | stores the letter as `<id>.placement` in the box of an office on this machine (the box is matched against the region file's offices for this machine; anything else is 404). The id comes from the letter, which must decode as a placement (400 if not); an id already in the box is 409; a box already holding 256 is 503 |
| `POST /replies/<asker>?id=<id>` | stores a reply as `<id>.reply` in `asker-<asker>`, a box that must already exist (404 otherwise: a stranger can't make the machine grow a directory per name) |
| `GET /catalog/<peer>` | the catalog `office publish` kept for that peer (under `var/published/<peer>/`), with the machine in `X-Office-Machine`; 404 if there is none |

A body is `text/plain` (charset utf-8 or none), at most 64 KiB for a
placement and 4 KiB for a reply (413 past it, counted while reading, so a
chunked body can't get past it either). A name is one path segment: an
escaped `/` or `..` is never a name. The endpoint stores into the same
on-disk boxes the `dir` transport uses, so the pull job is unchanged, and the
box's directory watch wakes it the moment the file is renamed into place: a
POST to `delivered` is a millisecond or two on one machine. It checks shape,
not identity: a letter is still trusted only for its signatures, which the
office checks when it pulls, so a forged one is stored and then dropped like
any other.

The client side: a peer's `address` is the URL of its machine's endpoint
(`http://` or `https://`, a host, no user, query or fragment; a path is a
prefix). `Send` and `Reply` POST with a 5 s timeout, three tries on a network
error or a 5xx (a 409 after a lost answer is the earlier try's letter, so
success), and never follow a redirect. `office publish` keeps each peer's
catalog locally for the endpoint to serve; bowery's resolver GETs the one
kept for its region. This region's own address (`""`) is the local boxes, as
for `dir`. One URL is one machine, so a peer region reached by http is one
machine for now. `office check` reports a peer address that isn't a URL, and
an http machine file without `listen`.

### Regions and peering

**Signatures.** Every signature is an SSH signature in the format
`ssh-keygen -Y sign` writes, made and checked by the binaries themselves
(ssh-ed25519 keys; the standard library is enough). Each kind has its own
namespace, so one can never pass as another: `placement` (the asker's),
`placement-region` (a region's seal), `offices-catalog` (a region's
catalog). A key with a passphrase or held in an agent is signed with by
`ssh-keygen` itself; every signature is checked natively.

**The asker signs.** `bowery place` signs the block with the asker's key:
`[placement] keys.<principal>`, or, for the invoking user, `~/.ssh/id_ed25519`.
Without one it places nothing (exit 1). The office checks the signature
against its signers file, which is `ssh-keygen`'s allowed_signers format
(`principals [namespaces="…"] ssh-ed25519 <key>`; patterns and other options
are refused, and `office check` reports a line that can never verify). A
namespaces list is comma-separated inside its quotes (`namespaces="placement,
offices-catalog"`): the option list splits only on commas outside quotes. A
principal of the office's own region is written bare; one from a peer region
`<principal>@<region>`, so the same name in two regions is two signers, and a
peer's principal can't pass as a local one by leaving out `from` (the key
wouldn't be listed bare). `asked_by` and `from` may not contain `@`.

**Peers.** A region file's `[peers.<region>]` gives the peer's `address` (the
transport's business), its pinned `key`, `may_place` (this region's offices
it may place into) and `[[peers.<region>.rules]]`, rule instances every
placement from it must also pass. A link exists only when both files declare
each other. A placement into a peer's office is sealed by the asker's region
(`[placement] region_key`, checked against its own region file's key before
it signs) and sent to the peer's address. The receiving office, after the
asker's signature, checks: the region is a peer, the seal verifies and is by
the pinned key, and the office is in `may_place`. Any of those failing is a
refusal, replied to the asker at the peer's address if the peer is still
declared (otherwise logged `reply-failed`). Then the usual checks, and the
accept-phase rules with the peer's rules run first (logged with source
`peer`), so when one of them closes, its reason and its `on_closed` are the
ones that count; a closed peer rule keeps the placement waiting like any
other rule.

**Revoking is one edit.** Every evaluation of a peer's job, at every phase,
also checks the link as it stands now: the entry is there, `may_place` still
names the office, and the pin is still the key that sealed the letter (read
again from the accepted letter). If not, the result is `closed` with an
integrity mark, so the owner's `open` doesn't override it: new placements
are refused, and running ones are held per the office's `on_closed` and
resume in place if the link comes back.

**Catalogs.** `office publish` (run by whoever may read the region key the
machine file names: open question 2) builds, for each declared peer, the
catalog of this machine's offices in that peer's `may_place`: holdings,
state, office-wide verdicts, running count, and the region's vocabulary
(priorities, reserved effects, `max_expires`), with no other jobs and no
local paths. It is signed with the region key, stamped `valid_until`
(`--valid`, default 10m), and published to the peer's address, replacing the
last one. A peer granted nothing gets an empty catalog, so narrowing reaches
it on the next publish; a peer struck from the file gets nothing, and its
office refusals do the rest. The resolver reads the catalogs a peer sent
(`consider.regions` names the peer), verifies each against the pin, and
plans over the offices listed. A catalog that doesn't verify is no link:
none of its offices is a candidate. A missing or stale one is a wait (the
peer may publish), not a setup gap. A peer's office is filtered on the
catalog alone (holdings against its region's vocabulary, its `max_expires`,
the owner's state, unexpired office-wide verdicts); there is no probe across
regions, and the office decides when the letter arrives. `bowery peers`
shows each peer's pin, its problems and what it published.

**Another region's clock.** The peer hop keeps the gate's clock rules. A
catalog published more than 5s ahead of the reader's clock, or an owner's
state in it dated ahead, is refused until it isn't (the job waits; the
offices aren't planned over). A peer placement signed ahead of the office's
clock is refused, since `max_expires` counts from the signing time. A retry
time that has already passed (a peer's verdict, a lagging slot) is never
booked as a retry: the resolver drops it, and the office's long-poll waits
for an event or the backstop instead of waking for it.

### Not yet

The wrapper and daemon, the `module` kind, the evaluator's own user,
`may_view`, catalogs published on an event (an office opening) rather than
when `office publish` runs, and peers' own published verdicts for their
peer rules. Rule plugins run as whoever runs the evaluation (the office's
user, in the pull job), and the resume re-check is the pull job's, not a
wrapper's.

## The bus

Offices exchange placements today; the bus is for everything else they
want to tell each other as it happens (a job started, a card was
answered), without anyone polling. Its core is `pkg/bus`, which other
programs import. The node that serves it and replication
between regions come in later pieces; this is what they build on.

- **One log per producer.** A producer is a region's seat
  (`<region>/<seat>`) or a service with a bare name. It numbers its own
  events with `seq`, strictly increasing, with gaps allowed (a service
  can number them by its own table's ids, where not every row goes out).
  There's no order across producers. An event that follows from another
  names it in `cause`.
- **Signed and sealed, like a placement.** Each event is signed by its
  producer (namespace `bus-event`) and sealed by its region
  (`bus-region`) over its canonical bytes: compact JSON, fields in a fixed
  order, free-form parts with sorted keys, the time in UTC. Neither
  signature can pass for the other, or for a placement or a catalog. A
  region's seat must be sealed by its own region. Both keys must be pinned
  by the reader. The signatures are `ssh-keygen -Y` signatures, so an
  event can be checked by hand.
- **A reader's position is a vector cursor**: one seq per producer,
  written `producer=seq,producer=seq` (sorted), so it fits in an SSE
  `Last-Event-ID`. A producer the cursor doesn't name is read from its
  start.
- **The log** keeps one append-only file per producer, one record per
  line. It verifies every record before appending it. A resend of a record
  it has is dropped quietly, so a producer that crashed between writing
  and sending just sends again. A different event at a seq it has is a
  fork, and one older than the head that it doesn't have is refused. Both
  are errors. A torn last line from a crash is cut off when the log opens,
  and any other bad line means the log won't open. Several handles may
  share a directory: each producer's file is locked while it's read or
  written, and a handle catches up with the others before it decides
  anything.
- **Topic rules, in the region file.** Each peer gets `may_publish` (the
  topics it may send into this region) and `may_read` (the topics it may
  be sent), beside `may_place`, so one edit grants or revokes both. Local
  seats get `publish` and `read` lists under `[bus.seats.<unix user>]`.
  Everything defaults to deny. A pattern is `*`, a prefix like `job.*`
  (whole parts, at any depth: not `jobs.started`), or one topic. Topics in
  `[bus] private` never leave the region, whatever a peer's `may_read`
  says, and no peer may publish into them; the region's own seats use them
  like any other. The check is one library call, `Rules.Allowed(peer,
  kind, dir)` (and `SeatAllowed` for seats), and the node makes it on both
  sides: before an event leaves for a peer, and again when one arrives, so
  a peer's own rules can't widen this region's. A bad pattern fails the
  region file, like any other bad value, and `office check` says which.
- **Retention per topic.** Events are kept 30 days unless `[bus] retention`
  or a pattern in `[bus.retain]` says otherwise (the most specific pattern
  wins). Compaction rewrites a producer's log without what's past its time
  and swaps it in by a rename; other handles see the new file the next
  time they lock it. It always keeps each producer's head, so a resend is
  still a duplicate and a different event at that seq still a fork. Each
  producer has a **floor**, the highest seq dropped, written beside its log
  before the swap (a crash in between leaves it too high, never too low).
  A cursor below a floor gets `ErrTooOld`, the bus's `410 resync`, and no
  records: the reader would miss something. A resyncing reader reads with
  `SinceKept`, which serves what's still kept instead, and once it has
  read to the heads it's past every floor.

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
