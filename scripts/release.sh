#!/bin/sh
# Tag a release and point the Homebrew formula at it.
#   scripts/release.sh v0.2.0
# Run from a clean main that's been pushed. It tags and pushes the tag,
# then updates Formula/berth.rb's url and sha256, commits and pushes that.
set -eu
v="${1:?usage: scripts/release.sh vX.Y.Z}"
case "$v" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "version must look like v1.2.3" >&2; exit 1;; esac
cd "$(dirname "$0")/.."
[ -z "$(git status --porcelain)" ] || { echo "commit or stash your changes first" >&2; exit 1; }
[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || { echo "release from main" >&2; exit 1; }
git fetch -q origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "push main first" >&2; exit 1; }
make test
make site
[ -z "$(git status --porcelain site)" ] || { echo "the docs site is out of date with README.md: commit what make site wrote" >&2; exit 1; }

git tag -a "$v" -m "Berth $v"
git push -q origin "$v"
url="https://github.com/dps/berth/archive/refs/tags/$v.tar.gz"
# GitHub makes the tarball on the first request; retry until it's there.
for i in 1 2 3 4 5; do
	sha=$(curl -fsSL "$url" | shasum -a 256 | cut -d' ' -f1) && break
	sleep 3
done
sed -i '' -e "s|^  url \".*\"|  url \"$url\"|" -e "s|^  sha256 \".*\"|  sha256 \"$sha\"|" Formula/berth.rb
git commit -q -m "Formula: berth $v" Formula/berth.rb
git push -q origin main
echo "Released $v ($sha). Upgrade with: berth upgrade"
