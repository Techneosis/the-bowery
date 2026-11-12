// Command bowery dispatches agents into herdr panes, local or on a saved
// herdr machine, and tears them down once their work is collected.
package main

import (
	"fmt"
	"os"
	"strings"
)

const usage = `bowery: send agents out into herdr, and bring them home with done.

  bowery dispatch <name> [placement] [options] [-- <prompt>]
  bowery done <name|pane|worktree-path>... [--force] [--keep-worktree] [--keep-branch] [--dry-run]
  bowery resume <name> [--swarm|--workspace] [--space LABEL] [--gated|--ungated] [--launcher CMD|--no-launcher]
                [--fresh] [--if-dead] [--not-within DUR] [--json] [--machine L] [--timeout MS] [--dry-run] [-- <prompt>]
  bowery watch <config> [--poll 30s] [--max 45m]   (bowery watch --help)

dispatch: make (or take) a pane, start the agent, tag it quiet, optionally
hand it a prompt, and record what was made for it. Prints JSON:
{"name","pane_id","workspace_id","tab_id","sound"}, plus "worktree" and
"branch" with --worktree, "machine" and "as" with --machine.

placement (default: --swarm)
  --swarm                  a tab labelled <name> in the swarm workspace, made
                           if absent: the home of agents torn down with done.
                           It closes with its last tab.
  --space <LABEL>          with --swarm: the tab goes in the workspace with
                           this label instead (made if absent, never moved).
                           Letters, digits, . _ -, up to 48; no spaces
  --split [right|down]     split the calling pane (or --from <ID>): an agent
                           to watch beside you
  --tab                    a new tab in the calling pane's workspace
  --workspace              a new workspace labelled <name>
  --pane <ID>              start in an existing pane sitting at a shell prompt
  --from <ID>              the pane to split / whose workspace to use
  --cwd <DIR>              working directory for a new pane (the repo, for
                           --worktree)

worktree (combines with any placement but --pane)
  --worktree <BRANCH>      a new git worktree of the repo at --cwd (default:
                           here) on a new branch, at <herdr worktrees dir>/
                           <repo>/<BRANCH> (slashes become dashes, as herdr
                           names them). The agent starts in it.
  --base <REF>             start point. Default: the remote's default branch,
                           freshly fetched (origin/main), never a stale main.
  --also-worktree <PATH>   an existing linked worktree this agent also works in
                           (repeatable): in use while the agent lives, torn
                           down with it. Later: adopt.

remote (a saved herdr machine: herdr machine list)
  --machine <LABEL>        dispatch on that machine's herdr server. herdr calls
                           go through herdr --machine, git and file checks
                           through ssh. Placement: --swarm, --workspace or
                           --pane. --cwd is a path on the box; ~/... and your
                           own $HOME/... map to the box's home.
  --as <WHO>               who the agent runs as there, through the machine's
                           identity_command (config). Required when the
                           machine has one, refused when it doesn't. Without
                           --machine: through the top-level identity_command,
                           for a box dispatching to its own server.

options
  --quiet                  tag sound=off (the default: dispatched agents are quiet)
  --loud                   no tag: this agent may beep when done or blocked
  --gated                  start with permission prompts on. The default is
                           bypass (claude gets --dangerously-skip-permissions):
                           an unwatched pane wedges silently on a prompt.
  --kind <KIND>            agent kind (default: claude)
  --model <MODEL>          model for a claude agent: an id or alias, passed
                           through (or expanded by [models] in the config)
  --launcher <CMD>         start claude through CMD (run in the pane's shell
                           with claude's arguments; it must exec claude),
                           for a wrapper that sets up its environment.
                           Recorded: resume reuses it
  --timeout <MS>           readiness timeout for agent start (default 60000,
                           at most 86400000: a day)
  --wait                   after the prompt, block until the agent settles

Claude's folder-trust dialog is answered Yes under the trust roots (config;
default ~/dev) and the herdr worktrees dir; anywhere else the start fails and
says so. A start that fails takes back whatever it made.

other commands
  done <target>...         tear an agent down: its pane, the tab or workspace
                           bowery made for it (every pane in it), its
                           worktrees (clean and merged only; squash merges
                           count) and their branches. Checks everything before
                           closing anything; refuses while the agent is
                           working or blocked. --force overrides the
                           working/busy-pane/dirty/unmerged checks (a forced
                           unmerged branch is kept). A remote agent is found
                           by its record; a path on a box needs --machine.
  resume <name>            start a dead agent again with its conversation
                           (claude --resume), in its worktree, on its
                           machine, as its --as: in its own pane if that is
                           at a shell, else a new pane in its tab, else a new
                           tab in swarm (--workspace for a workspace),
                           through its recorded --launcher (--launcher CMD
                           sets one, --no-launcher drops it). Its
                           first message says it was resumed. Refuses,
                           changing nothing, while it or its session is
                           alive; without a session or directory in its
                           record; for a kind other than claude; when its
                           directory or a worktree is gone; when the pane it
                           would use is someone else's or carries a
                           hands_off_env marker; when its --as no longer
                           matches the config; with --swarm/--workspace
                           while its old tab is open; and for a pre-0.3.0
                           record without --gated or --ungated. --dry-run
                           says where.
                           For a hook or a job: --if-dead does nothing (exit
                           0) when the agent is alive or another resume,
                           dispatch, adopt or done of it is running;
                           --not-within DUR refuses one resumed that
                           recently. One of those runs on a name at a time.
                           --fresh starts it in a new conversation instead,
                           in the same place and the same way (no session
                           needed; the record takes the new one). --json
                           prints one line whatever happens, with an
                           "outcome": resumed, started_fresh, alive, busy,
                           too_soon, dry_run, refused or failed.
  adopt <name> [<path>...] [--launcher CMD] [--gated|--ungated]
                           record more worktrees as <name>'s; or give a live
                           agent started by hand a record resume can use (its
                           session and folder, plus the launcher and the
                           permission mode it was started with); or fill an
                           old record with no session or folder from its
                           agent, alive in the record's own terminal
  orphans [--machine L] [--json]
                           what done would clean up (and which dead agents
                           resume can bring back), on every server with a
                           record here. Read-only. An unreachable machine is
                           a warning, and an "unreachable" entry in --json.
  list [--machine L]       agents, who is muted, dead agents that can be
                           resumed, and an orphan count
  mute|unmute <target>...  set or clear an agent's sound=off tag
  swarm-tab <label> [--cwd DIR] [--machine L]
                           a new tab in swarm with no agent; prints its pane
  swarm-last               move swarm to the bottom of the workspace list
  config                   the settings in force, and where they come from
  upgrade [--check]        install the latest release (--check only asks)
  version

Records live in ~/.local/state/bowery/; the config is
~/.config/bowery/config.toml. See docs/design.md for the why of all of it.`

func main() {
	argv := os.Args[1:]
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" || argv[0] == "help" {
		fmt.Println(usage)
		return
	}
	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "version", "--version":
		fmt.Println(versionString())
		return
	case "upgrade":
		check := false
		for _, a := range rest {
			if a != "--check" {
				die("upgrade: unknown option %s", a)
			}
			check = true
		}
		cmdUpgrade(check)
		return
	}
	if cmd == "resume" {
		resumeJSONScan(rest) // so a bad config still answers --json with one line
	}
	cfg = loadConfig()
	switch cmd {
	case "dispatch":
		cmdDispatch(rest)
	case "done":
		cmdDone(rest)
	case "resume":
		cmdResume(rest)
	case "adopt":
		cmdAdopt(rest)
	case "orphans":
		cmdOrphans(rest)
	case "list":
		cmdList(rest)
	case "mute", "unmute":
		cmdMute(cmd, rest)
	case "swarm-tab":
		cmdSwarmTab(rest)
	case "swarm-last":
		if err := swarmLast(); err != nil {
			die("%v", herdrMsg(err))
		}
	case "config":
		cmdConfig(rest)
	case "names":
		cmdNames()
	case "watch":
		cmdWatch(rest)
	default:
		hint := ""
		if nameRE.MatchString(cmd) {
			hint = fmt.Sprintf("; to dispatch an agent named %s: bowery dispatch %s %s", cmd, cmd, strings.Join(rest, " "))
		}
		die("unknown command %s%s (bowery --help)", cmd, strings.TrimRight(hint, " "))
	}
}
