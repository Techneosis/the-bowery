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
| `cmd/bowery/watch.go` | `watch`: the background watcher, its kinds and `watch.d` scripts |
| `cmd/bowery/upgrade.go`, `version.go` | `upgrade [--check]` and the version string |
| `cmd/bowery/herdr.go` | the herdr CLI and socket, and the group-safe pane close |
| `cmd/bowery/machine.go` | `--machine`: the current server, ssh, paths on the box |
| `cmd/bowery/records.go` | dispatch records, and which server a name lives on |
| `cmd/bowery/place.go` | `place`, `queue`, `office`, `peers`, and dispatch's job flags: the resolver, signing and sealing, peers' catalogs |
| `cmd/office/` | the office host: `open/close [--now]/clear`, `describe` (and `--probe`), `pull` (the run phase, then the inbox; `--loop` long-polls), `stop/done/yielded`, `record` (the recorder), `log`, `check`, `ls`, `publish` (catalogs to peers), `serve` (the http transport's endpoint) |
| `pkg/bus/` | the office bus's core, importable by other programs: the event envelope and its canonical bytes (`event.go`), the producer's signature and the region's seal, vector cursors (`cursor.go`), the per-producer append-only log and its retention (`log.go`), the topic rules (`rules.go`; their config is `internal/offices/topics.go`), the local node: publish as a seat, follow with a cursor, back-pressure (`node.go`; `office bus` in `cmd/office/bus.go`, the peer's Unix user in `peercred_*.go`), replication between regions: signed push and pull calls, per-peer cursors, the socket and http links (`peer.go`; `office serve`'s `/bus/peer` in `internal/offices/http.go`). See "The bus" in `docs/design.md` |
| `cmd/rules/hours/` | the reference rule plugin (exec kind, contract 1) |
| `internal/offices/` | shared by both: config files, state, placements, holdings checks, the evaluator, state-file gates, facts, running work and the run phase, the transport, the pull job, the root-mode guards, SSH signatures (`sshsig.go`, `ssh-keygen -Y` format, ed25519, stdlib only), letters and signers (`letter.go`), peer catalogs (`catalog.go`), the logs (`log.go`), the root and dev-mode layouts (`paths.go`), the http transport and its endpoint (`http.go`), the inbox wait (`watch_linux.go` inotify, `watch_darwin.go` kqueue, `watch_other.go` polling) |
| `completions/` | bash completion, for `bowery` and `office` (installed under both names) |
| `agents/` | what ships to users' Claude Code: the skill, and the CLAUDE.md snippet |
| `examples/config.toml` | every config key, with its default |
| `examples/offices/` | one region, machine and office, annotated, at their root-mode paths (adapt them under `OFFICE_ROOT/etc` to try it: the machine name must be this host's) |
| `docs/releases/` | per-release notes, used by the release workflow instead of generated ones |
| `install.sh` | piped/downloaded fetches a release, a checkout builds, an unpacked release installs its binary; `--claude-only` touches only Claude Code |
| `scripts/package.sh` | builds and packages every release target, plus `SHA256SUMS` |
| `scripts/test-suite` | the black-box python suite in parallel: every test dealt across processes (four per CPU), the binaries built once; about a minute, against 9 in one process. CI and `ci-local` run it |
| `scripts/ci-local` | the CI suite on a commit, here and on a second machine over ssh (`BOWERY_CI_REMOTE`), in parallel, failing fast: run it before every push |
| `tests/test_offices.py` | offices in dev mode (`OFFICE_ROOT`), with the real hours plugin and a scripted one |
| `tests/test_offices_run.py` | rules on the machine: state-file gates, facts, the run phase and `on_closed`, owner overrides, the long-poll |
| `tests/test_offices_regions.py` | two dev-mode regions on one machine: signed and sealed placements, peer rules, pinned keys, catalogs, revocation, stage times across the hop (keys from `tests/_keys.py`, signed with the real `ssh-keygen`) |
| `tests/test_offices_bus.py` | the local bus node in dev mode: `office bus serve`, publish as a seat, follow (cursor, topics, the wake), status, one node per log, the region key |
| `tests/test_offices_bus_peers.py` | two dev-mode regions' bus nodes replicating, over the dir and http transports: both sides' rules, one side making both calls, catch-up after a peer was down, a stranger's key refused |
| `tests/test_offices_http.py` | the regions story again over the http transport (two `office serve` endpoints, URL addresses), plus a replay over the wire, the endpoint's limits and the wake |
| `tests/test_offices_rv.py` | M2's review findings: future-dated gates, a past `retry_after`, mid-pass events, `done` during the run phase |
| `tests/test_offices_review.py` | integrity beats the owner, the decisions log and recorder, fd exec, process groups, replay replies (root-mode halves in `internal/offices/rootmode_test.go`) |
| `tests/` | `python3 -m unittest discover tests` and `go test ./...`: black-box, fake herdr and ssh (`tests/fakes/`), real git. Run before every commit |
| `tests/live/dispatch.sh` | the same story against a real throwaway herdr server and real claude agents; run by hand when herdr changes or the fake does |
| `docs/design.md` | the plain-language version: why each piece is the way it is |

## Rules

- **Push only what passed on both platforms.** `scripts/ci-local` runs the
  CI suite on the commit here and on a machine of the other platform
  (`BOWERY_CI_REMOTE=user@host`, a clone at `BOWERY_CI_REMOTE_DIR`) at once.
  A GitHub run takes 12-16 minutes, so it is the record, not the test loop:
  branch pushes run Linux alone there, and macOS runs on main, tags and pull
  requests.
- **Time is a bound, not the claim.** A test of "it wakes on the event" or
  "it doesn't wait on a timer" asserts the mechanism (the timer it could
  have waited on is long, and it's seen waiting before the event) with a
  generous bound, never a tight number of milliseconds: shared runners are
  slow. Log the measured time instead.
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
