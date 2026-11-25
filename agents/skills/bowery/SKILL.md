---
name: bowery
description: Dispatch herdr agents with bowery: start them (locally or on a saved herdr machine), tag them quiet, prompt them, collect results, and tear them down with `bowery done`. Use whenever the user says "dispatch" (an agent, a reviewer, a red-teamer, N workers), whenever you decide to hand part of a task to a separate herdr agent instead of an in-process subagent, whenever a dispatched agent finishes (tear it down), whenever a dispatched agent died or was closed with its work unfinished (resume it), whenever asked what's been left behind (orphans), whenever work should go to an office instead of a pane (place), and whenever agents, PRs or CI runs in flight need watching (watch). Covers the one command to use, the swarm workspace, worktrees, remote dispatch with --machine/--as, the sound tag, naming, prompting and waiting, teardown, resuming a dead agent, placing a job into an office and reading its exit, bowery watch and its watch.d kinds, and the herdr worktree-group hazard.
---

# Dispatching agents with bowery

"Dispatch" means: start a separate herdr agent (its own pane, its own Claude
session) and hand it work. Not the Agent tool: that is an in-process subagent
and needs none of this.

## The one command

```bash
bowery dispatch <name> [placement] [--worktree BRANCH [--base REF]] [--cwd DIR]
                [--also-worktree PATH]... [--loud] [--gated] [--model MODEL] [--launcher CMD]
                [--machine LABEL [--as WHO]] [--wait] [-- <prompt>]
bowery done <name>        # when its result is collected
```

Always use it instead of raw `herdr pane split` + `herdr agent start`. It does
pane → `agent start` (with the shell-not-ready retry, and Claude's
folder-trust dialog answered for folders under the configured trust roots and
the herdr worktrees dir) → **tag** → prompt in one call, records the dispatch,
and prints `{"name","pane_id","workspace_id","tab_id","sound"}` (plus
`worktree`, `branch`, and `machine`/`as` when they apply). A start that fails
takes back the pane, tab or worktree it made, so it leaves no half-started
orphan.

Placement, default **`--swarm`**:

| flag | pane goes | use it for |
|---|---|---|
| `--swarm` (default) | a tab labelled `<name>` in the `swarm` workspace (made if absent) | everything you will tear down with `done`: workers, reviewers, red-teamers |
| `--split [right\|down]` | split off your pane (`--from <pane>` to split another) | an agent the user wants to watch beside them |
| `--tab` | a new tab in your workspace, labelled `<name>` | same, less cramped |
| `--workspace [--cwd DIR]` | a new workspace labelled `<name>` | a long-lived agent that earns its own sidebar entry |
| `--pane <id>` | an existing pane sitting at a shell prompt | reusing a pane you already made |

`--space <label>` puts a swarm tab in that workspace instead, for this call
(made if absent, closed with its last tab, never moved). It goes only with
`--swarm`. A label is letters, digits, `.`, `_` or `-`, up to 48, so a
workspace whose label has a space can't be named; a misspelt label makes a
new workspace. On `resume` it names where a new tab goes when the agent's own
pane and tab are gone; it doesn't stop a resume from using those first.

Swarm keeps dispatched agents out of the user's own workspaces: one sidebar
entry, one tab per agent, moved to the bottom of the workspace list when
bowery makes or finds it. It closes when its last agent is torn down and
comes back on the next dispatch. Pick another placement only when the user
asked to see the agent, or it should outlive your session.

**Worktrees: `--worktree <BRANCH> [--base REF] --cwd <repo>`** makes a fresh
checkout at `<herdr worktrees dir>/<repo>/<branch>` on a new branch from the
freshly fetched remote default branch (not a stale local main), and starts the
agent in it, in a swarm tab by default (any placement but `--pane` works).
**Don't use `herdr worktree create` for dispatched agents**: see the hazard
below.

**An agent that works in more than one checkout** (an app repo and its API
repo, say) must have every worktree on its record, or `orphans` calls the
second one "no pane is in it" and `done <path>` would remove it from under a
live agent. Declare an existing worktree at dispatch with
`--also-worktree <PATH>` (repeatable), or, if you make it after dispatching,
`bowery adopt <name> <PATH>...`. A recorded worktree is in use while its agent
lives, pane or no pane, and `done <name>` tears it down with the agent's own
under the same merged-and-clean guards.

**Dispatched agents bypass permissions by default**: claude gets
`--dangerously-skip-permissions`. A dispatched pane is unwatched, so a
permission prompt doesn't fail loudly; it wedges the agent silently until your
`herdr agent wait` times out. `--gated` opts out: the agent starts with
permission prompts on. Use it deliberately for work where a human gate is the
point (production, live stores, outbound sends), and then expect dialogs:
watch with `herdr agent read` and answer with `herdr agent send-keys`, or the
agent sits forever. Kinds other than claude get no permissions flag either
way.

`--model <MODEL>` starts the agent on a specific model: a full id or an alias
claude understands (`opus`, `sonnet`, `haiku`), passed through as written
unless the user's config maps it (`[models]`). Omit it and the agent uses its
own default. Only valid with `--kind claude` (the default).

`--launcher '<command>'` starts claude through a wrapper of the user's, when
the agent must be started with an environment of its own (a seat launcher,
say). The command runs in the pane's shell with claude's arguments after it,
and must `exec claude "$@"`. bowery sets `BOWERY_LAUNCH=1` for it, and
sends the first prompt itself, so a launcher with an opening prompt of its
own should skip it then. It's recorded, so `bowery resume` uses it again;
`bowery resume <name> --launcher '<command>'` adds one to an agent that had
none.

## Sound: why the tag matters

When many agents run, the user wants to hear only the **orchestrators** finish
or block. Dispatched agents are tagged `sound=off` by default (a herdr pane
token, which a sound plugin of the user's skips). Use `--loud` only when the
user should be paged by *this* agent directly (rare: usually you, the
orchestrator, relay). You stay loud: that is the signal the user is listening
for, so finish your turn or ask your question rather than sitting idle.

Retag later: `bowery mute|unmute <name|pane>...`; `bowery list` shows who is
on and off.

## Names

Lowercase, digits, `-`, `_`. Short and role-shaped: `rt-code`, `inv-data`,
`editor`, `reviewer-2`. Names are how you address the agent afterwards.

## Prompting and collecting

- `-- <prompt>` submits right after start. Add `--wait` to block until the
  agent settles (idle, done or blocked). Without it, poll later with
  `herdr agent wait <name> --until done --until blocked --timeout <ms>`.
- dispatch then checks the agent started on the prompt: still idle after
  60s, it gets one more Enter, and still idle 15s after that, dispatch exits
  1 with "agent <name> never started working; its prompt may be
  unsubmitted". The agent is left up: `herdr agent read <name>` to see its
  composer. `--no-start-check` skips this, for an agent you mean to leave
  idle.
- Follow-ups: `herdr agent prompt <name> "<text>" [--wait]`. It refuses with
  `agent_blocked` if the agent is at a dialog: `herdr agent read <name>` to
  see what it wants, `herdr agent send-keys` to answer.
- `herdr agent read <name>` returns only the visible screen for a full-screen
  agent, so ask dispatched agents to **write results to a file** and tell you
  the path, not to print them.
- Give each agent the full context it needs in the prompt; it shares nothing
  with you but the filesystem.
- For a remote agent, add `--machine <LABEL>` to those herdr commands.

## Teardown: the standing rule

**Tear each dispatched agent down as soon as you have verified its result:
one at a time, not in a sweep at the end of the batch.** An agent left idle is
an orphan: a pane, a worktree, a branch and maybe a blank repo-root workspace
that the user has to find and clean up by hand.

```bash
bowery done <name>            # or a pane id, or a worktree path
bowery done <name> --dry-run  # what it would do
bowery orphans                # what's been left behind
```

`done` runs every check before it changes anything, and is safe to re-run:

- **Refuses while the agent is `working` or `blocked`.** Read it first.
- **Closes the agent's pane, and everything it opened in its own tab or
  workspace.** With `--swarm`, `--tab` or `--workspace` bowery made that
  tab/workspace for the agent and recorded it, so every pane in it is the
  agent's: done closes them all and the tab/workspace with them (swarm itself
  with its last tab). A pane there running something rather than sitting at a
  shell prompt makes it refuse; `--force` closes it too. With
  `--split`/`--pane` the agent sits among panes that aren't its own, so only
  its pane is closed. If the agent is gone but its tab is still open,
  `orphans` says so and `done <name>` closes it.
- **Its worktrees:** removed, and the branch deleted, only when the checkout
  is clean and the branch is merged into the remote default branch (squash and
  rebase merges count). Otherwise it refuses and says why. `--force` removes a
  dirty or unmerged checkout but *keeps* an unmerged branch; `--keep-worktree`
  closes just the pane. It never removes a worktree another pane is still in,
  or one another live agent's record claims (`done <path>` included: it says
  whose, and `done <that-agent>` is the way).
- **A blank repo-root workspace** (`herdr worktree create` leaves one,
  labelled with the repo name) is closed only when it's an idle shell and no
  worktree workspace of that repo is still open.
- **Fast-forwards the main checkout** when it is clean and on the default
  branch.

Teardown order for a merge-based task: agent pushes → PR merged → `done`. If
it refuses "not merged", the PR isn't merged yet; don't reach for `--force`
unless the work is meant to be thrown away.

**Tickets.** When the work has a ticket (a tracker key like `DEV-551`, or a
work-item id), pass `--ticket <ID>` to `dispatch`: it's recorded, `resume`
keeps it, and `bowery list --json` reports it, which is how a dashboard ties
a running agent to its ticket. One per agent; letters, digits, `.`, `_`, `-`,
up to 80.

**`bowery list --json`** is the program's view: one JSON array on stdout, an
object per agent on every server `list` covers, with `name`, `machine` (null
for the local server), `as`, `state` (`working`, `idle`, `done`, `blocked`,
or `gone` for a record whose agent is dead), `title`, `ticket`, `worktree`,
`branch`, `dispatched_at`, `last_seen` (now for a live agent, null for a gone
one), plus `pane`, `sound`, `dispatched`, `resumable`, `resumes` and `note`.
Absent values are null. A machine that can't be read is one entry with
`state: "unreachable"`, never an empty list. No orphan count: that's
`orphans --json`.

`bowery list` prints a one-line orphan count on stderr; `bowery orphans` lists
them: finished dispatched agents still alive, worktrees with no pane in them
that no live agent's record claims, blank repo-root workspaces, records whose
agent is gone (and their leftover tab, if any). Clean up only what you
dispatched; anything else, tell the user.

## Resume rather than re-dispatch

When a dispatched agent's pane died, herdr restarted, the machine rebooted,
or it was closed by mistake, its work is half done and its conversation is
the only record of how far it got. Bring it back instead of starting over:

```bash
bowery resume <name> [--dry-run] [--space LABEL] [--gated|--ungated] [--launcher CMD|--no-launcher] [-- <what to do next>]
```

It starts the agent again in the same worktree, machine and `--as` identity,
resuming its Claude session, in its own pane if that is still at a shell
prompt, else a new pane in its own tab, else a new swarm tab (`--workspace`
for a workspace; `--space LABEL` puts that new tab in the workspace with that
label, and gives a `--workspace` agent a tab there instead). Its first message tells it it was resumed and to check what
it already did before redoing anything; add your own follow-up after `--`.
Then wait on it and `done` it as usual.

- **Resume** when the work is unfinished and the worktree still holds it:
  the agent knows what it tried, what failed and what it already sent.
- **Re-dispatch** (`done` it, then `dispatch` afresh) when the job changed, the
  conversation went wrong, or the agent finished and you only need a new task.
- **An agent that places paid or irreversible work** (a review contract, a
  deploy, a sent message) must check that work's status after a resume, not
  place it again. Say so in the follow-up if it matters.

It refuses, changing nothing, while the agent or its session is alive; when
its record has no session or directory (an older dispatch never seen alive
since: `bowery resume <name> --fresh`, or `bowery adopt <name> --gated|--ungated`
while that agent is still alive in its own pane, which fills both from it);
when its recorded launcher isn't one plain command line (`--launcher CMD` or
`--no-launcher`); for a non-claude agent; when its directory or a worktree is gone; when
the pane it would use holds someone else's agent, a running process or another
tool's marker (`hands_off_env`); when its `--as` no longer matches the config;
with `--swarm`/`--workspace` while its old tab is still open; and with
`--space` given with `--workspace`, twice with different labels, or with a
label that breaks the rule above. A record
from before 0.3.0 doesn't say whether the agent was gated: pass `--gated` or
`--ungated`. `--ungated` on a gated record is for that start only (the JSON
line says `"ungated_once": true`). `--timeout MS` is at most a day (86400000).
`bowery orphans` marks dead agents that can be resumed with
`(bowery resume <name>; done ends that)`, and `bowery list` shows them as
`gone ... bowery resume <name>`.

**`done` ends a resume.** `done <name>` drops the record and its session, and
`done <path>` on its worktree removes the worktree resume needs (and drops the
record of an agent that ran in it). Resume first if the work isn't finished.

**From a hook or a job** (start a seat when mail comes):
`bowery resume <name> --if-dead --not-within 10m -- "<why>"`. `--if-dead`
does nothing when it's alive or already being resumed; `--not-within` keeps
a seat that keeps dying from being restarted in a loop. `--fresh` starts it
in a new conversation instead (a long-idle seat whose old context would cost
more to reload than it's worth).

If a resume fails after the agent started ("is running again in ..."), the
record already points at it: send the notice yourself, or `done` it.

## Placing a job into an office

Where offices are set up (an `office` host and a region file on the
machine), work can go to an office instead of a pane. **Which to use:**

- **`dispatch`** (no job flags) when *you* start the agent and will watch it,
  collect it and `done` it: it runs now, as you, in a pane you choose.
- **`place`** (or `dispatch <name>` with a job flag) when the work belongs to
  an office: it must run as that office's user, with what it holds (repos,
  tools, secrets), and only when its rules let it (office hours, a gate, a
  budget). You don't pick the pane or the time; the office does, and its
  surface starts the agent. You don't `done` a placed job either.

```bash
bowery place --kind <kind> [--needs tools=git] [--pin <office>] [--ticket <ID>] -- "<prompt>"
bowery dispatch <name> --job-kind <kind> [--office <office>] [--ticket <ID>] -- "<prompt>"
```

In `dispatch`, `--kind` stays the agent kind (claude); the job's kind is
`--job-kind`, and `--office`, `--scope`, `--needs`, `--asked-by`,
`--priority`, `--pin`, `--tag` or `--surface` also make it a placement.
Then only `--worktree`, `--base` and `--ticket` may go with them, and
`--machine` is refused (`--office` pins it instead). `--ticket <ID>` is
carried on the job as its `ticket` tag. The office's own rules decide when it
takes the job. Both print one JSON line; read the exit, never re-implement a
rule:

- **0** placed: `{"placed": {"office", "machine", "region", "id", "why",
  "skipped", "stages"}}`. `id` is the job id.
- **4** waiting: `{"queued": {"job", "waiting_for", "retry_after",
  "told"}}`. `waiting_for` is `"<office>: <reason>"` (or "no office
  answers"), `retry_after` when the office said when it might open, `told`
  who was just told (the asker, and the closed office's owner). Tell the
  person that, then leave it: `bowery place --retry` (run on a timer, or when
  the office opens) sends it on, and `bowery queue` lists what's waiting.
  Don't dispatch it elsewhere by hand.
- **3** `{"refused": {"job", "reason"}}`: a reserved effect, refused by
  policy. It needs a person.
- **1** an error, or `{"unplaceable": {"job", "reason", "candidates"}}`: no
  office holds what it needs. That's a setup gap to report, not something to
  retry.

Once placed, the office keeps watching: if its rules or its owner close it,
a running job is held where it is (its work kept) and resumes there when it
opens. A held job is never yours to re-place: `bowery office describe
<office>` shows it held and why, and `bowery office log <office> [--since
1h]` what happened. Only the owner ends one (`bowery office stop <office>
<job>`).

Every placement is signed with the asker's key (`[placement] keys` in
bowery's config, or your own `~/.ssh/id_ed25519`): "no signing key for X" is
a setup gap to report, not a reason to place as someone else. Offices in
another region are reached only when both regions declare each other;
`bowery peers` shows each peer, whether its catalog verifies, and the
offices it lets this region use. An office not listed there doesn't exist
for you: don't try to reach it another way.

## Watching what's in flight (`bowery watch`)

When you have agents, PRs or CI runs in flight, don't poll them in a loop
of your own. Write what you're waiting on to a file and run `bowery watch
<file>` in the background: it polls (`--poll 30s`) and **exits** as soon as
something worth acting on changes, so its exit is your wake-up. Then act,
update the file to what's in flight now, and start it again.

```
repo owner/app                 # for the pr and run lines below
machine swarm-box              # agent lines below are on that machine ("local" = this one)
agent fixer                    # idle | done | blocked | gone (working is quiet)
pr 412                         # OPEN | MERGED | CLOSED, with /CONFLICTING
run ci.yml feature-x           # the latest run on a branch or commit (queued, in_progress quiet)
ticket T-12                    # any other kind: ~/.config/bowery/watch.d/ticket T-12
```

It prints `CHANGED:` (the lines that changed) and `ALL:` (every line), or
`STILL BLOCKED:` on its first poll when an agent is already at a dialog, or
`HEARTBEAT` after `--max` (45m) with nothing worth waking for: re-arm it
then too. State is kept beside the file (`<file>.state`), so a re-armed
watch wakes only on what changed since the last one. Every kind reports a
coarse state, never progress text.

**A kind of your own** is an executable at `~/.config/bowery/watch.d/<kind>`,
run as `<kind> <key...>` with `BOWERY_WATCH_MACHINE` and `BOWERY_WATCH_REPO`
set. Its last line of stdout is the value; a leading `~` marks that value
quiet (it never wakes the watch on its own); a non-zero exit or no output
is a transient `?`, which never wakes it. A kind with no script is
`unknown-kind`; a line added since the last run starts quiet, so wakes only
when it changes after that. `bowery watch
--help` has the details.

## Remote dispatch (`--machine`, `--as`)

For work on a shared box running its own herdr server, saved locally as a
herdr machine (`herdr machine list`):

```bash
bowery dispatch <name> --machine <LABEL> --as <WHO> \
    [--worktree BRANCH --cwd ~/<path to repo on the box>] [-- <prompt>]
bowery done <name>     # no flag: found by its record
```

- Every herdr call for that agent goes through `herdr --machine <LABEL>`, and
  every git or file check through `ssh` (the profile's target, or `ssh =` in
  the config). The box's herdr server must already be running; bowery never
  installs or starts anything there.
- **Identities:** when the machine has an `identity_command` in
  `~/.config/bowery/config.toml`, `--as <who>` is required and picks who the
  agent works as (whose git author and GitHub token); without one, `--as` is
  refused and the agent runs as the ssh user. bowery turns the pane's shell
  into `<identity_command> <who>`, checks the identity's variable in it, then
  starts the agent, so the credentials stay on the box. Remote git (fetch,
  worktree add, done's merge checks) runs as `<who>` too. An unknown identity
  is refused before anything is made. Run `bowery config` to see what is set.
- **Placement:** `--swarm` (default; a swarm workspace on the box),
  `--workspace` or `--pane`. `--split`, `--tab` and `--from` are refused:
  they mean beside your local pane.
- **Paths are on the box.** `~/...` (and your own `$HOME/...`, if your shell
  expanded it) map to the box's home. `--worktree` needs `--cwd`, and the repo
  must already be cloned there; bowery says so and does not clone.
- **Records** carry `machine`, `host` and `as`. `done`, `mute` and `adopt`
  find a remote agent by name without a flag (a local name wins a clash; pass
  `--machine` to pick). A worktree *path* on the box needs `--machine`.
  `list` and `orphans` cover the local server plus every machine you have a
  record for (`--machine X` for just that one). A box is shared, so there
  only agents with a record made *here* count as yours: someone else's quiet
  agents are not your orphans, and a worktree with no record says "maybe
  another dispatcher's". Don't `done` those.

## The worktree-group hazard (why not `herdr worktree create`)

`herdr worktree create` groups the new workspace under a **primary** workspace
on the repo's main checkout: a blank one labelled with the repo name if none
is open, or the `--workspace` you name (making *your own* workspace the
primary). **`herdr pane close` on the primary's last pane closes the whole
group**, killing every agent still working in a sibling worktree.
`workspace close <primary>` refuses instead. So:

- dispatch worktrees with `--worktree` (a plain `git worktree add`, no group);
- never `herdr pane close` the last pane of a repo-root workspace: use
  `bowery done`, or `herdr workspace close`, which refuses when it would
  cascade;
- never `herdr workspace close --group` to get past that refusal.
