#!/usr/bin/env bash
# Install The Bowery (`bowery`) for the current user.
#
#   curl -fsSL https://raw.githubusercontent.com/Techneosis/the-bowery/main/install.sh | bash
#
# Run that way (or as a lone downloaded file) it fetches the latest release for
# this machine, checks its SHA-256, unpacks it into
# ~/.local/share/bowery/dist/<version>/, and installs from there; running it
# again upgrades. Options go after `bash -s --`, e.g. `| bash -s -- --claude`.
# Run from a checkout, it builds from source instead, and links into the
# checkout so edits are live.
#
#   ./install.sh                link the binary and completion, set up Claude Code
#   ./install.sh --version v0.1.0   that release rather than the latest (download only)
#   ./install.sh --claude       ...and add the CLAUDE.md section without asking
#   ./install.sh --no-claude    skip Claude Code entirely
#   ./install.sh --claude-only  only the Claude Code part (no binary)
#   ./install.sh --uninstall    remove all of it (records and config stay)
#   ./install.sh --uninstall --claude-only   remove only the Claude Code part
#   ./install.sh --no-build     install build/bowery as it is (a release
#                               binary, or one cross-compiled elsewhere)
#
# Claude Code: when ~/.claude (or $CLAUDE_CONFIG_DIR) exists, the bowery skill
# is linked into its skills/, and a short routing section is offered for its
# CLAUDE.md. That section sits between marker comments, so re-running updates
# it in place and --uninstall takes it out again. CLAUDE.md is written in
# place, never replaced, so a CLAUDE.md that is itself a symlink stays one.
#
# Nothing is written to ~/.config/bowery: every setting has a default, and
# `bowery config` shows them. See README.md for the file.
set -euo pipefail

repo=Techneosis/the-bowery
releases=${BOWERY_RELEASES:-https://github.com/$repo/releases}
data=${XDG_DATA_HOME:-$HOME/.local/share}/bowery

# Where this script runs from decides what it installs. Piped into bash it has
# no file at all, and the working directory says nothing about it.
if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]]; then
  src=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
else
  src=""
fi
if [[ -n $src && -f $src/cmd/bowery/main.go ]]; then
  mode=checkout
elif [[ -n $src && -f $src/VERSION && -x $src/build/bowery ]]; then
  mode=release
else
  mode=download
fi
bin=$HOME/.local/bin
comp=${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions
claude=${CLAUDE_CONFIG_DIR:-$HOME/.claude}

BEGIN='<!-- bowery:begin (managed by the-bowery/install.sh; edit agents/claude-md-snippet.md upstream, not here) -->'
END='<!-- bowery:end -->'

uninstall=0 claude_only=0 claude_md=ask build=1 want_version=latest
[[ $mode == release ]] && build=0
passthrough=()
while (($#)); do
  arg=$1; shift
  case $arg in
    --version)     want_version=${1:?--version needs a release, e.g. v0.1.0}; shift; continue ;;
    --version=*)   want_version=${arg#*=}; continue ;;
    --uninstall)   uninstall=1 ;;
    --claude-only) claude_only=1 ;;
    --claude)      claude_md=yes ;;
    --no-claude)   claude_md=never ;;
    --no-build)    build=0 ;;
    -h|--help)     sed -n '2,29p' "${BASH_SOURCE[0]:-/dev/null}" 2>/dev/null | sed 's/^# \{0,1\}//'
                   [[ -z $src ]] && echo "see https://github.com/$repo#readme"; exit 0 ;;
    *) echo "unknown option: $arg (try --help)" >&2; exit 2 ;;
  esac
  passthrough+=("$arg")
done

# ---------------------------------------------------------------- download

platform() {
  local o a
  case $(uname -s) in Linux) o=linux ;; Darwin) o=darwin ;;
    *) echo "bowery runs on Linux and macOS, not $(uname -s)" >&2; exit 1 ;; esac
  case $(uname -m) in x86_64|amd64) a=amd64 ;; aarch64|arm64) a=arm64 ;;
    *) echo "no bowery build for $(uname -m)" >&2; exit 1 ;; esac
  echo "$o-$a"
}

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

download_and_install() {
  local plat asset url tmp want got ver
  plat=$(platform)
  asset=bowery-$plat.tar.gz
  if [[ $want_version == latest ]]; then url=$releases/latest/download; else url=$releases/download/$want_version; fi
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  echo "fetching $asset ($want_version)"
  curl -fsSL "$url/$asset" -o "$tmp/$asset" || { echo "could not download $url/$asset" >&2; exit 1; }
  curl -fsSL "$url/SHA256SUMS" -o "$tmp/SHA256SUMS" || { echo "could not download $url/SHA256SUMS" >&2; exit 1; }
  want=$(awk -v a="$asset" '$2 == a || $2 == "*"a {print $1}' "$tmp/SHA256SUMS")
  got=$(sha256 "$tmp/$asset")
  if [[ -z $want || $want != "$got" ]]; then
    echo "checksum mismatch for $asset: expected ${want:-nothing}, got $got. Not installing." >&2
    exit 1
  fi
  echo "checksum ok"
  tar -xzf "$tmp/$asset" -C "$tmp"
  ver=$(cat "$tmp/bowery-$plat/VERSION")
  mkdir -p "$data/dist"
  rm -rf "${data:?}/dist/$ver"
  mv "$tmp/bowery-$plat" "$data/dist/$ver"
  # Everything links through dist/current, so an upgrade is one swapped link.
  local previous=""
  [[ -L $data/dist/current ]] && previous=$(readlink "$data/dist/current")
  ln -sfn "$ver" "$data/dist/current"
  # Keep this release and the one before it; older ones go.
  for d in "$data"/dist/*/; do
    d=$(basename "$d")
    [[ $d == current || $d == "$ver" || $d == "$previous" ]] || rm -rf "${data:?}/dist/$d"
  done
  echo "unpacked $ver into $data/dist/$ver"
  exec bash "$data/dist/current/install.sh" "${passthrough[@]+"${passthrough[@]}"}"
}

if [[ $mode == download && $uninstall == 0 ]]; then
  download_and_install
fi
if [[ $mode != download && $want_version != latest ]]; then
  echo "--version is for downloading a release; this is a $mode install" >&2
  exit 2
fi

links=(
  "$src/build/bowery:$bin/bowery"
  "$src/completions/bowery.bash:$comp/bowery"
)

link() {  # link <from> <to>, refusing to clobber anything that isn't a link
  local from=$1 dst=$2
  mkdir -p "$(dirname "$dst")"
  if [[ -e $dst && ! -L $dst ]]; then
    echo "refusing to replace $dst: it is a real file, not a link" >&2
    exit 1
  fi
  ln -sfn "$from" "$dst"
  echo "linked $dst"
}

# Rewrite a file through any symlink: content in place, never a rename.
rewrite() {  # rewrite <file> <awk program> [awk args...]
  local f=$1 prog=$2 tmp; shift 2
  tmp=$(mktemp)
  awk "$@" "$prog" "$f" >"$tmp" && cat "$tmp" >"$f"
  rm -f "$tmp"
}

has_block() { [[ -f $1 ]] && grep -qF '<!-- bowery:begin' "$1"; }

claude_install() {
  [[ $claude_md == never ]] && return 0
  if [[ ! -d $claude ]]; then
    echo "Claude Code not found at $claude; skipping its setup (CLAUDE_CONFIG_DIR points elsewhere)"
    return 0
  fi
  link "$src/agents/skills/bowery" "$claude/skills/bowery"

  local md=$claude/CLAUDE.md
  if has_block "$md"; then
    rewrite "$md" '
      index($0, "<!-- bowery:begin") == 1 { print b; while ((getline l < snip) > 0) print l; skip = 1; next }
      index($0, "<!-- bowery:end") == 1   { skip = 0; print e; next }
      !skip' -v b="$BEGIN" -v e="$END" -v snip="$src/agents/claude-md-snippet.md"
    echo "updated the bowery section of $md"
    return 0
  fi

  if [[ $claude_md == ask ]]; then
    # Piped from curl, stdin is the script; a person at a terminal still has
    # /dev/tty, and an unattended run has neither.
    local from=""
    if [[ -t 0 ]]; then from=/dev/stdin; elif [[ -t 1 ]] && { : </dev/tty; } 2>/dev/null; then from=/dev/tty; fi
    if [[ -n $from ]]; then
      echo
      echo "Add a short section to $md so Claude dispatches herdr agents with bowery"
      echo "(rather than raw herdr pane/agent commands that leave orphans behind)?"
      read -r -p "[Y/n] " reply <"$from"
      [[ -z $reply || $reply == [Yy]* ]] || { echo "skipped; ./install.sh --claude adds it later"; return 0; }
    else
      echo "not adding a CLAUDE.md section without asking; run the installer again with --claude to add it"
      return 0
    fi
  fi
  { [[ -s $md ]] && echo; echo "$BEGIN"; cat "$src/agents/claude-md-snippet.md"; echo "$END"; } >>"$md"
  echo "added a bowery section to $md"
}

claude_uninstall() {
  [[ -L $claude/skills/bowery ]] && rm "$claude/skills/bowery" && echo "removed $claude/skills/bowery"
  if has_block "$claude/CLAUDE.md"; then
    # Also drops the one blank line install put before the section.
    rewrite "$claude/CLAUDE.md" '
      held { if (index($0, "<!-- bowery:begin") != 1) print ""; held = 0 }
      index($0, "<!-- bowery:begin") == 1 { skip = 1; next }
      index($0, "<!-- bowery:end") == 1   { skip = 0; next }
      skip { next }
      $0 == "" { held = 1; next }
      { print }
      END { if (held) print "" }'
    echo "removed the bowery section from $claude/CLAUDE.md"
  fi
}

case $uninstall$claude_only in
  01) claude_install; exit 0 ;;
  11) claude_uninstall; exit 0 ;;
  10)
    for pair in "${links[@]}"; do
      dst=${pair#*:}
      [[ -L $dst ]] && rm "$dst" && echo "removed $dst"
    done
    claude_uninstall
    if [[ -d $data/dist ]]; then rm -rf "${data:?}/dist" && echo "removed $data/dist"; fi
    echo "uninstalled; dispatch records (~/.local/state/bowery) and config (~/.config/bowery) are left alone"
    exit 0 ;;
esac

case $(uname -s) in
  Linux|Darwin) ;;
  *) echo "bowery runs on Linux and macOS, not $(uname -s)" >&2; exit 1 ;;
esac

if ((build)); then
  command -v go >/dev/null \
    || { echo "building bowery from a checkout needs Go (https://go.dev/dl/); or put a binary at build/bowery and pass --no-build" >&2; exit 1; }
  (cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/bowery.new ./cmd/bowery)
  mv -f "$src/build/bowery.new" "$src/build/bowery"
  echo "built $src/build/bowery"
elif [[ ! -x $src/build/bowery ]]; then
  echo "--no-build, but there is no binary at $src/build/bowery" >&2
  exit 1
fi
"$src/build/bowery" version >/dev/null \
  || { echo "$src/build/bowery doesn't run here (built for another platform?)" >&2; exit 1; }

for pair in "${links[@]}"; do
  link "${pair%%:*}" "${pair#*:}"
done
command -v herdr >/dev/null || echo "note: herdr is not on your PATH; bowery drives it (https://herdr.dev)"
[[ :$PATH: == *":$bin:"* ]] || echo "note: $bin is not on your PATH"

claude_install
