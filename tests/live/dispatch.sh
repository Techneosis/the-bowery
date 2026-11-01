#!/usr/bin/env bash
#
# bowery against a REAL herdr server and real claude agents (on haiku), run by
# hand: the fakes in tests/ stand in for herdr's behaviour, and this is how to
# check they still match it.
#
#   tests/live/dispatch.sh            # needs herdr and claude on PATH
#
# Runs against its OWN herdr server (a throwaway named session), throwaway git
# repos with a bare "origin", and its own records, worktrees and config, so it
# never sees, let alone closes, a pane in the session you work in. Everything
# it makes lives under one temp dir and goes away on exit. Remote dispatch is
# covered by tests/test_remote.py only (it needs a second machine).
#
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
D="${BOWERY_BIN:-}"
if [[ -z $D ]]; then
  D="$(mktemp -d)/bowery"
  (cd "$HERE/../.." && CGO_ENABLED=0 go build -o "$D" ./cmd/bowery) || exit 1
fi
SESSION="bowery-live-$$"
TMP="$(mktemp -d -t bowery-live.XXXXXX)"
TMP="$(cd "$TMP" && pwd -P)"
pass=0; fail=0

cleanup() {
  herdr session stop "$SESSION" >/dev/null 2>&1
  herdr session delete "$SESSION" >/dev/null 2>&1
  rm -rf "$TMP"
}
trap cleanup EXIT

ok()   { pass=$((pass+1)); printf '  ok   %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  FAIL %s\n' "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2" | sed 's/^/       /'; }
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }
has()  { local d=$1 hay=$2 needle=$3; if [[ "$hay" == *"$needle"* ]]; then ok "$d"; else bad "$d" "wanted: $needle"$'\n'"got: $hay"; fi; }
hasnt(){ local d=$1 hay=$2 needle=$3; if [[ "$hay" != *"$needle"* ]]; then ok "$d"; else bad "$d" "did not want: $needle"$'\n'"got: $hay"; fi; }

# ---- an isolated herdr server. Nested herdr refuses to start with HERDR_* set.
for v in $(env | sed -n 's/^\(HERDR[A-Z_]*\)=.*/\1/p'); do unset "$v"; done
unset TERM_PROGRAM
herdr --session "$SESSION" server >"$TMP/server.log" 2>&1 &
export HERDR_SOCKET_PATH="$HOME/.config/herdr/sessions/$SESSION/herdr.sock"
for _ in $(seq 50); do herdr workspace list >/dev/null 2>&1 && break; sleep 0.2; done
herdr workspace list >/dev/null 2>&1 || { echo "test herdr server did not come up"; cat "$TMP/server.log"; exit 1; }

export BOWERY_STATE="$TMP/state"
export BOWERY_WORKTREE_ROOT="$TMP/worktrees"
export BOWERY_CONFIG="$TMP/config.toml"
export BOWERY_TRUST_ROOTS="$TMP"
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t

REPO="$TMP/repo"; OTHER="$TMP/other"   # OTHER plays GitHub merging the PR
git init -q --bare -b main "$TMP/origin.git"
git clone -q "$TMP/origin.git" "$REPO" 2>/dev/null
echo base >"$REPO/file"; git -C "$REPO" add file; git -C "$REPO" commit -qm init; git -C "$REPO" push -q origin main
git -C "$REPO" remote set-head origin main >/dev/null
git clone -q "$TMP/origin.git" "$OTHER"

ws_labels() { herdr workspace list | python3 -c 'import json,sys; print(" ".join(w["label"] for w in json.load(sys.stdin)["result"]["workspaces"]))'; }
agent_names() { herdr agent list | python3 -c 'import json,sys; print(" ".join(a.get("name") or "-" for a in json.load(sys.stdin)["result"]["agents"]))'; }
agent_field() { herdr agent list | python3 -c 'import json,sys
for a in json.load(sys.stdin)["result"]["agents"]:
    if a.get("name")==sys.argv[1]: print(eval(sys.argv[2]))' "$1" "$2"; }
commit_in() { echo "$2" >"$1/$2"; git -C "$1" add "$2"; git -C "$1" commit -qm "$2"; git -C "$1" push -q origin HEAD 2>/dev/null; }
merge_pr() {  # merge_pr <branch> merge|squash   — in OTHER, then push main
  git -C "$OTHER" fetch -q origin; git -C "$OTHER" checkout -q main; git -C "$OTHER" merge -q --ff-only origin/main
  if [[ $2 == squash ]]; then git -C "$OTHER" merge -q --squash "origin/$1" >/dev/null && git -C "$OTHER" commit -qm "squash $1"
  else git -C "$OTHER" merge -q --no-ff -m "merge $1" "origin/$1"; fi
  git -C "$OTHER" push -q origin main
}
start() { local n=$1; shift; "$D" dispatch "$n" "$@" --model haiku --timeout 60000 2>&1; }

echo "== start --worktree, default placement swarm (folder-trust answered, tagged, recorded)"
out="$(BOWERY_TRUST_ROOTS="" start wt-merged --worktree wt-merged --cwd "$REPO")"
has "start prints the worktree" "$out" "\"worktree\": \"$TMP/worktrees/repo/wt-merged\""
has "agent is up under its name" "$(agent_names)" "wt-merged"
has "agent is tagged sound=off" "$(agent_field wt-merged 'a.get("tokens",{}).get("sound")')" "off"
check "dispatch record written" test -n "$(ls "$BOWERY_STATE"/wt-merged.*.json 2>/dev/null)"
check "worktree branched from origin/main" test "$(git -C "$TMP/worktrees/repo/wt-merged" rev-parse HEAD)" = "$(git -C "$REPO" rev-parse origin/main)"
swarm_ws() { herdr workspace list | python3 -c 'import json,sys
for w in json.load(sys.stdin)["result"]["workspaces"]:
    if w["label"]=="swarm": print(w["workspace_id"], "grouped" if w.get("worktree") else "plain", w["tab_count"])'; }
tab_labels() { herdr tab list --workspace "$1" | python3 -c 'import json,sys; print(" ".join(t["label"] for t in json.load(sys.stdin)["result"]["tabs"]))'; }
read -r SW kind ntabs <<<"$(swarm_ws)"
check "a swarm workspace was made" test -n "$SW"
check "swarm is not a herdr worktree group" test "$kind" = plain
has "agent got its own tab, labelled with its name" "$(tab_labels "$SW")" "wt-merged"
check "and the blank tab swarm came with is gone" test "$ntabs" = 1
hasnt "no repo-root primary spawned" "$(ws_labels)" "repo"

echo "== done: merged branch -> pane, worktree, branch gone; main fast-forwarded"
commit_in "$TMP/worktrees/repo/wt-merged" a.txt; merge_pr wt-merged merge
out="$("$D" done wt-merged 2>&1)"; rc=$?
check "done exits 0" test $rc -eq 0
has "its tab closed" "$out" "closed tab"
hasnt "agent gone" "$(agent_names)" "wt-merged"
check "worktree removed" test ! -e "$TMP/worktrees/repo/wt-merged"
check "branch deleted" test -z "$(git -C "$REPO" branch --list wt-merged)"
check "main checkout fast-forwarded" test "$(git -C "$REPO" rev-parse HEAD)" = "$(git -C "$REPO" rev-parse origin/main)"
check "record dropped" test -z "$(ls "$BOWERY_STATE"/wt-merged.*.json 2>/dev/null)"
out="$("$D" done wt-merged 2>&1)"; rc=$?
check "re-run is a no-op success" test $rc -eq 0
has "re-run says so" "$out" "nothing to tear down"

echo "== done: squash-merged counts as merged; dirty main is not fast-forwarded"
start wt-squash --worktree wt-squash --cwd "$REPO" >/dev/null
commit_in "$TMP/worktrees/repo/wt-squash" b.txt; commit_in "$TMP/worktrees/repo/wt-squash" c.txt; merge_pr wt-squash squash
echo local-edit >>"$REPO/file"
out="$("$D" done wt-squash 2>&1)"; rc=$?
check "done exits 0" test $rc -eq 0
has "squash recognised" "$out" "content already in origin/main"
check "branch deleted" test -z "$(git -C "$REPO" branch --list wt-squash)"
has "dirty main left alone" "$out" "not fast-forwarded: uncommitted changes"
git -C "$REPO" checkout -q -- file

echo "== done: unmerged refuses; --force removes checkout, keeps branch"
start wt-unmerged --worktree wt-unmerged --cwd "$REPO" >/dev/null
commit_in "$TMP/worktrees/repo/wt-unmerged" d.txt
out="$("$D" done wt-unmerged 2>&1)"; rc=$?
check "refused (exit 1)" test $rc -eq 1
has "says why" "$out" "is not merged into origin/main"
has "agent untouched" "$(agent_names)" "wt-unmerged"
out="$("$D" done wt-unmerged --force 2>&1)"; rc=$?
check "forced done exits 0" test $rc -eq 0
check "worktree removed" test ! -e "$TMP/worktrees/repo/wt-unmerged"
check "unmerged branch kept" test -n "$(git -C "$REPO" branch --list wt-unmerged)"
has "and it says so" "$out" "kept branch wt-unmerged"

echo "== done: dirty refuses; --keep-worktree closes only the pane; done <path> finishes"
start wt-dirty --worktree wt-dirty --cwd "$REPO" >/dev/null
touch "$TMP/worktrees/repo/wt-dirty/scratch"
out="$("$D" done wt-dirty 2>&1)"; rc=$?
check "refused (exit 1)" test $rc -eq 1
has "says why" "$out" "uncommitted or untracked"
"$D" done wt-dirty --keep-worktree >/dev/null 2>&1
hasnt "pane closed" "$(agent_names)" "wt-dirty"
check "worktree kept" test -e "$TMP/worktrees/repo/wt-dirty/scratch"
out="$("$D" orphans 2>&1)"
has "orphans lists the paneless worktree" "$out" "$TMP/worktrees/repo/wt-dirty  [wt-dirty, dirty]"
rm "$TMP/worktrees/repo/wt-dirty/scratch"
out="$("$D" done "$TMP/worktrees/repo/wt-dirty" 2>&1)"; rc=$?
check "done <path> exits 0" test $rc -eq 0
check "worktree removed" test ! -e "$TMP/worktrees/repo/wt-dirty"

echo "== a branch with a slash gets herdr's dash-named checkout"
start wt-slash --worktree feat/slashy --cwd "$REPO" >/dev/null
check "checkout at repo/feat-slashy" test -d "$TMP/worktrees/repo/feat-slashy"
out="$("$D" done wt-slash 2>&1)"; rc=$?
check "done exits 0" test $rc -eq 0
check "branch feat/slashy deleted" test -z "$(git -C "$REPO" branch --list feat/slashy)"

echo "== done: a working agent is refused; idle agents show as orphans"
start busy --workspace --cwd "$REPO" -- "Use the Bash tool to run exactly: sleep 25. Then reply: finished." >/dev/null
herdr agent wait busy --until working --timeout 30000 >/dev/null 2>&1
out="$("$D" done busy 2>&1)"; rc=$?
check "refused (exit 1)" test $rc -eq 1
has "says still working" "$out" "still working"
herdr agent wait busy --until idle --until done --timeout 90000 >/dev/null 2>&1
has "orphans lists the finished agent" "$("$D" orphans 2>&1)" "busy"
out="$("$D" done busy 2>&1)"; rc=$?
check "done after it finished exits 0" test $rc -eq 0
hasnt "its workspace is gone" "$(ws_labels)" "busy"

echo "== start failure leaves nothing behind (trust dialog outside the trusted roots)"
mkdir -p "$TMP/untrusted"
out="$(BOWERY_TRUST_ROOTS="" BOWERY_WORKTREE_ROOT="$TMP/elsewhere" start nope --workspace --cwd "$TMP/untrusted")"; rc=$?
check "start fails" test $rc -ne 0
has "says why" "$out" "outside"
hasnt "its workspace was closed" "$(ws_labels)" "nope"

echo "== swarm: closing one agent's tab never touches another; swarm goes with its last tab"
start sw-a --cwd "$REPO" >/dev/null
start sw-b --worktree sw-b --cwd "$REPO" >/dev/null
read -r SW _ ntabs <<<"$(swarm_ws)"
check "both agents share one swarm" test "$ntabs" = 2
"$D" done sw-a >/dev/null 2>&1
has "sw-b still alive" "$(agent_names)" "sw-b"
check "sw-b still idle and answering" test "$(agent_field sw-b 'a["agent_status"]')" = idle
has "sw-b's tab intact" "$(tab_labels "$SW")" "sw-b"
hasnt "sw-a's tab gone" "$(tab_labels "$SW")" "sw-a"
"$D" done sw-b >/dev/null 2>&1
check "swarm closed with its last tab" test -z "$(swarm_ws)"
check "sw-b's worktree removed" test ! -e "$TMP/worktrees/repo/sw-b"

echo "== --split beside a host pane: done closes only that pane"
hostp="$(herdr workspace create --label host --cwd "$REPO" --no-focus | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["root_pane"]["pane_id"])')"
start wt-split --split down --from "$hostp" --worktree wt-split --cwd "$REPO" >/dev/null
has "agent is up" "$(agent_names)" "wt-split"
"$D" done wt-split >/dev/null 2>&1
has "host workspace survived" "$(ws_labels)" "host"
check "worktree removed" test ! -e "$TMP/worktrees/repo/wt-split"
herdr workspace close "${hostp%%:*}" >/dev/null

pane_of() { agent_field "$1" 'a["pane_id"]'; }
split_off() { herdr pane split --pane "$1" --direction down --no-focus | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["pane"]["pane_id"])'; }
pane_ids() { herdr pane list | python3 -c 'import json,sys; print(" ".join(p["pane_id"] for p in json.load(sys.stdin)["result"]["panes"]))'; }
term_of() { herdr pane get "$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["pane"]["terminal_id"])'; }
terms() { herdr pane list | python3 -c 'import json,sys; print(" ".join(p["terminal_id"] for p in json.load(sys.stdin)["result"]["panes"]))'; }

echo "== done closes every pane an agent opened in its own swarm tab, and the tab"
start keep --cwd "$REPO" >/dev/null
start sp-a --cwd "$REPO" >/dev/null
extra="$(split_off "$(pane_of sp-a)")"; xt="$(term_of "$extra")"; sleep 1
read -r SW _ ntabs <<<"$(swarm_ws)"
out="$("$D" done sp-a 2>&1)"; rc=$?
check "done exits 0" test $rc -eq 0
has "says it closed the tab and both panes" "$out" "with 2 pane(s)"
hasnt "the extra blank shell is gone too" "$(terms)" "$xt"
hasnt "sp-a's tab is gone" "$(tab_labels "$SW")" "sp-a"
has "the other swarm agent's tab is untouched" "$(tab_labels "$SW")" "keep"
check "and that agent is still idle" test "$(agent_field keep 'a["agent_status"]')" = idle

echo "== ... but a pane in it that is running something needs --force"
start sp-b --cwd "$REPO" >/dev/null
live="$(split_off "$(pane_of sp-b)")"; sleep 1
herdr pane run "$live" "sleep 301" >/dev/null; sleep 1
out="$("$D" done sp-b 2>&1)"; rc=$?
check "refused (exit 1)" test $rc -eq 1
has "names the busy pane" "$out" "not an idle shell prompt"
has "agent untouched" "$(agent_names)" "sp-b"
check "its process untouched" pgrep -f "^sleep 301$"
out="$("$D" done sp-b --force 2>&1)"; rc=$?
check "forced done exits 0" test $rc -eq 0
sleep 1; check "the process went with the tab" bash -c '! pgrep -f "^sleep 301$"'

echo "== an agent gone from its tab leaves the tab as an orphan; done <name> closes it"
start sp-c --cwd "$REPO" >/dev/null
stray="$(split_off "$(pane_of sp-c)")"; st="$(term_of "$stray")"; sleep 1
herdr pane close "$(pane_of sp-c)" >/dev/null
has "orphans reports the leftover tab" "$("$D" orphans 2>&1)" "sp-c: agent gone, its tab"
out="$("$D" done sp-c 2>&1)"; rc=$?
check "done exits 0" test $rc -eq 0
hasnt "the stray pane is gone" "$(terms)" "$st"
"$D" done keep >/dev/null 2>&1

echo "== --workspace: the agent's extra panes go with its workspace"
start ws-a --workspace --cwd "$REPO" >/dev/null
split_off "$(pane_of ws-a)" >/dev/null; sleep 1
out="$("$D" done ws-a 2>&1)"; rc=$?
check "done exits 0" test $rc -eq 0
has "closed the workspace and both panes" "$out" "with 2 pane(s)"
hasnt "workspace gone" "$(ws_labels)" "ws-a"

echo "== --split beside a host: panes around the agent are not its own"
hostp="$(herdr workspace create --label host2 --cwd "$REPO" --no-focus | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["root_pane"]["pane_id"])')"
start sp-split --split down --from "$hostp" --cwd "$REPO" >/dev/null
nbr="$(split_off "$(pane_of sp-split)")"; nt="$(term_of "$nbr")"; ht="$(term_of "$hostp")"; sleep 1
out="$("$D" done sp-split 2>&1)"; rc=$?
check "done exits 0" test $rc -eq 0
has "host pane survived" "$(terms)" "$ht"
has "the pane split off the agent survived too (not provably its)" "$(terms)" "$nt"
herdr workspace close "${hostp%%:*}" >/dev/null

echo "== a second worktree, declared at dispatch (--also-worktree) or later (adopt)"
REPO2="$TMP/repo2"
git init -q --bare -b main "$TMP/origin2.git"
git clone -q "$TMP/origin2.git" "$REPO2" 2>/dev/null
echo base >"$REPO2/file"; git -C "$REPO2" add file; git -C "$REPO2" commit -qm init; git -C "$REPO2" push -q origin main
git -C "$REPO2" remote set-head origin main >/dev/null
W2="$TMP/worktrees/repo2/feat-two"; mkdir -p "$TMP/worktrees/repo2"
git -C "$REPO2" worktree add -q -b feat/two "$W2" origin/main
start two --worktree two --cwd "$REPO" --also-worktree "$W2" >/dev/null
has "recorded in the dispatch record" "$(cat "$BOWERY_STATE"/two.*.json)" "$W2"
hasnt "orphans does not call it paneless while its agent lives" "$("$D" orphans 2>&1)" "$W2"
out="$("$D" done "$W2" 2>&1)"; rc=$?
check "done <path> refuses (exit 1)" test $rc -eq 1
has "naming the live agent" "$out" "belongs to live agent two"
check "worktree untouched" test -d "$W2"
out="$("$D" done two 2>&1)"; rc=$?
check "done <name> exits 0" test $rc -eq 0
check "its own worktree removed" test ! -e "$TMP/worktrees/repo/two"
check "and the second one" test ! -e "$W2"
check "second branch deleted (merged)" test -z "$(git -C "$REPO2" branch --list feat/two)"

W3="$TMP/worktrees/repo2/feat-three"
start three --cwd "$REPO" >/dev/null
git -C "$REPO2" worktree add -q -b feat/three "$W3" origin/main
has "before adopt it looks orphaned" "$("$D" orphans 2>&1)" "$W3"
out="$("$D" adopt three "$W3" 2>&1)"; rc=$?
check "adopt exits 0" test $rc -eq 0
hasnt "after adopt it is in use" "$("$D" orphans 2>&1)" "$W3"
"$D" adopt three "$W3" >/dev/null 2>&1
check "adopt is idempotent" test "$(grep -c "$W3" "$BOWERY_STATE"/three.*.json)" = 1
commit_in "$W3" e.txt
out="$("$D" done three 2>&1)"; rc=$?
check "unmerged adopted worktree refuses the whole done" test $rc -eq 1
has "agent untouched by the refusal" "$(agent_names)" "three"
git -C "$W3" reset -q --hard origin/main
out="$("$D" done three 2>&1)"; rc=$?
check "then done exits 0" test $rc -eq 0
check "adopted worktree removed" test ! -e "$W3"

W4="$TMP/worktrees/repo2/feat-four"
git -C "$REPO2" worktree add -q -b feat/four "$W4" origin/main
has "a worktree no record names and no pane is in is still an orphan" "$("$D" orphans 2>&1)" "$W4"
"$D" done "$W4" >/dev/null 2>&1
check "and done <path> removes it" test ! -e "$W4"

echo "== swarm stays at the bottom of the workspace list"
start sl-a --cwd "$REPO" >/dev/null
last_ws() { herdr workspace list | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["workspaces"][-1]["label"])'; }
check "the dispatch put swarm last" test "$(last_ws)" = swarm
herdr workspace create --label after1 --cwd "$TMP" --no-focus >/dev/null
"$D" swarm-last; rc=$?
check "swarm-last puts it back last" test "$(last_ws)" = swarm
"$D" swarm-last; rc=$?
check "swarm-last when already last is a quiet no-op" test $rc -eq 0
"$D" done sl-a >/dev/null 2>&1
herdr workspace create --label after3 --cwd "$TMP" --no-focus >/dev/null; sleep 1
check "with no swarm, nothing is moved" test "$(last_ws)" = after3
for l in after1 after3; do
  id="$(herdr workspace list | python3 -c 'import json,sys
for w in json.load(sys.stdin)["result"]["workspaces"]:
    if w["label"]==sys.argv[1]: print(w["workspace_id"])' "$l")"; herdr workspace close "$id" >/dev/null
done

echo "== hazard: herdr-made worktree group; a sibling must survive every done"
mk() { herdr worktree create --cwd "$REPO" --branch "$1" --base origin/main --path "$TMP/worktrees/repo/$1" --label "$1" --no-focus \
       | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["root_pane"]["pane_id"])'; }
h1="$(mk h1)"; h2="$(mk h2)"
prim="$(herdr workspace list | python3 -c 'import json,sys
for w in json.load(sys.stdin)["result"]["workspaces"]:
    if w.get("worktree") and not w["worktree"]["is_linked_worktree"]: print(w["workspace_id"]+":p1")')"
check "herdr spawned a primary on the main checkout" test -n "$prim"
herdr pane run "$h2" "sleep 300" >/dev/null; sleep 1
alive() { pgrep -f "^sleep 300$" >/dev/null; }
out="$("$D" orphans 2>&1)"
has "orphans flags the primary as leave-it" "$out" "leave it"
out="$("$D" done "$h1" 2>&1)"; rc=$?
check "done of one sibling exits 0" test $rc -eq 0
has "primary left open while h2 is open" "$out" "left repo-root workspace"
check "h2's process survived" alive
out="$("$D" done "$prim" 2>&1)"; rc=$?
check "closing the primary's last pane is refused" test $rc -eq 1
has "says it would close the group" "$out" "would close them too"
check "h2's process survived that too" alive
out="$("$D" done "$h2" --force 2>&1)"; rc=$?
check "done of the last sibling exits 0" test $rc -eq 0
has "and closes the blank primary" "$out" "closed blank repo-root workspace"
hasnt "no repo workspaces left" "$(ws_labels)" "repo"

echo
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
