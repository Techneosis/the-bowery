#!/usr/bin/env bash
# Build and package every release target: scripts/package.sh <version> <outdir>
#
# Each archive, bowery-<os>-<arch>.tar.gz, unpacks to a directory that
# install.sh recognises as a release (a VERSION file and build/bowery), so the
# installer inside it installs that binary rather than building one.
# SHA256SUMS covers every archive; VERSION names the release, for
# `bowery upgrade`. The release workflow runs this; so can you.
set -euo pipefail

version=${1:?usage: scripts/package.sh <version> <outdir>}
out=${2:?usage: scripts/package.sh <version> <outdir>}
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*} arch=${target#*/}
  name=bowery-$os-$arch
  dir=$work/$name
  mkdir -p "$dir/build"
  (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$dir/build/bowery" ./cmd/bowery)
  cp -R "$root/completions" "$root/agents" "$root/examples" "$dir/"
  mkdir -p "$dir/docs"
  cp "$root/install.sh" "$root/LICENSE" "$root/README.md" "$dir/"
  cp "$root/docs/design.md" "$dir/docs/"
  echo "$version" >"$dir/VERSION"
  tar -C "$work" -czf "$out/$name.tar.gz" "$name"
  echo "packaged $name.tar.gz"
done
(cd "$out" && { command -v sha256sum >/dev/null && sha256sum bowery-*.tar.gz || shasum -a 256 bowery-*.tar.gz; } >SHA256SUMS)
echo "wrote $out/SHA256SUMS"
# What `bowery upgrade` reads, from <releases>/latest/download/VERSION.
echo "$version" >"$out/VERSION"
echo "wrote $out/VERSION"
