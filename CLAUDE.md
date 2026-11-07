# Working on The Bowery

This file is for working **on** The Bowery. The instructions for **using** it
(what an agent should do when asked to dispatch something) are the skill in
`agents/skills/bowery/SKILL.md`, which `install.sh` links into a user's Claude
Code setup.

## Layout

| path | what |
|---|---|
| `cmd/bowery/` | the whole tool, in Go: one static binary (`CGO_ENABLED=0`) |
| `cmd/bowery/dispatch.go` | `dispatch`: worktree, pane, identity, agent start, trust dialog, tag, record; `swarm-tab` |
| `cmd/bowery/done.go` | `done` (checks, then teardown) and `adopt` |
| `cmd/bowery/orphans.go` | `orphans` (read-only), `list`, `mute`/`unmute` |
| `cmd/bowery/resume.go` | `resume`: its checks, where it lands, the hands-off check |
| `cmd/bowery/watch.go` | `watch`: the background watcher and its kinds |
| `cmd/bowery/herdr.go` | the herdr CLI and socket, and the group-safe pane close |
| `cmd/bowery/machine.go` | `--machine`: the current server, ssh, paths on the box |
| `cmd/bowery/records.go` | dispatch records, and which server a name lives on |
| `completions/` | bash completion |
| `agents/` | what ships to users' Claude Code: the skill, and the CLAUDE.md snippet |
| `examples/config.toml` | every config key, with its default |
| `install.sh` | piped/downloaded fetches a release, a checkout builds, an unpacked release installs its binary; `--claude-only` touches only Claude Code |
| `scripts/package.sh` | builds and packages every release target, plus `SHA256SUMS` |
| `tests/` | `python3 -m unittest discover tests`: black-box, fake herdr and ssh (`tests/fakes/`), real git. Run before every commit |
| `tests/live/dispatch.sh` | the same story against a real throwaway herdr server and real claude agents; run by hand when herdr changes or the fake does |
| `docs/design.md` | the plain-language version: why each piece is the way it is |

## Rules

- **Run the tests.** They drive the real binary against throwaway records
  (`BOWERY_STATE`), worktrees (`BOWERY_WORKTREE_ROOT`), config
  (`BOWERY_CONFIG`) and a fake herdr server, so they never touch a real
  session. A behaviour change comes with a test that fails without it.
- **The fake is held to herdr.** `tests/fakes/herdr` answers in the real
  CLI's shapes and models the behaviour bowery depends on (not-ready shells,
  the trust dialog, group cascades, process-info). If bowery starts relying
  on something new from herdr, teach the fake, and run `tests/live/dispatch.sh`
  to check the fake still tells the truth.
- **Check everything before changing anything.** `done` runs all its refusals
  before its first close. Keep it that way: a refused `done` must have changed
  nothing.
- **Never make a herdr worktree group**, and never `pane close` a last pane
  without going through `safeClosePane`. See "The worktree-group hazard" in
  `docs/design.md`.
- **An ssh failure is never an answer.** Exit 255/126/127 from ssh is an
  error, not "not a worktree" or "not merged".
- **Records are a contract.** Existing records must keep loading, including
  ones written by older versions (no `owns`, `branch: null`). A format change
  keeps reading the old one and comes with a test against it.
- **One static binary.** Go's standard library plus
  `github.com/BurntSushi/toml` (pure Go). A new dependency has to keep
  `CGO_ENABLED=0` builds working for every target in `scripts/package.sh`.
- **The README stays in the frame**: the network, the Rooftop, the book,
  strays, across the river. Every metaphor maps to real behaviour; if the
  behaviour changes, so does the README. `docs/design.md` is the plain version.
- **This repo is public.** Nothing machine-, person- or client-specific goes
  in any file, commit message or PR: no home paths, private script names,
  client names, hostnames or account names. Examples use generic values
  (`swarm-box`, `alice`, `~/src/app`). A pointer that only matters on one
  machine belongs in that machine's config, not here.
- **`agents/` is for everyone.** Change the snippet only in
  `agents/claude-md-snippet.md`; installed copies are rewritten from it.
- **Commit messages carry no AI attribution.**

## Cutting a release

1. Optionally write `docs/releases/vX.Y.Z.md` (otherwise notes are generated
   from the commits). Commit it.
2. `git tag vX.Y.Z && git push origin vX.Y.Z`. A tag with a `-`
   (`v0.2.0-rc1`) publishes as a prerelease, which the installer's "latest"
   skips.
3. `.github/workflows/release.yml` packages all four targets, verifies them on
   Linux amd64, Linux arm64 and macOS (checksums, the version string, and the
   whole suite against the packaged binary), and only then publishes.

To rehearse without publishing: `scripts/package.sh v0.0.0-test /tmp/rel/latest/download`
and `BOWERY_RELEASES=file:///tmp/rel bash -s -- --no-claude < install.sh`
(with a throwaway `HOME`, and `XDG_DATA_HOME` unset, unless you mean to
install it: the installer honours `XDG_DATA_HOME`).
