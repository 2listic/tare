#!/bin/sh
# Builds the release archives into dist/, and with --publish puts them on a
# GitHub release.
#
#   ./tools/release.sh v0.1.0             build and check, publish nothing
#   ./tools/release.sh v0.1.0 --publish   tag HEAD, push the tag, create the release
#
# Archive names carry no version, so the latest release always has the same
# download URL and the install line in README.md never goes stale:
# kubectl-tare_<os>_<arch>.tar.gz (.zip on Windows), each holding the binary,
# LICENSE and README.md. checksums.txt has their SHA-256.
#
# A dirty tree is refused: a released binary has to match a commit, or the
# version it prints names code that never existed.
set -eu

cd "$(dirname "$0")/.."

version=${1:-}
publish=${2:-}
case "$version" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) echo "usage: $0 vX.Y.Z [--publish]" >&2; exit 2 ;;
esac
case "$publish" in
"" | --publish) ;;
*) echo "usage: $0 vX.Y.Z [--publish]" >&2; exit 2 ;;
esac

if [ -n "$(git status --porcelain)" ]; then
	echo "working tree is dirty; commit or stash first" >&2
	exit 1
fi

targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

go test ./...

rm -rf dist bin/kubectl-tare-*
mkdir -p dist
# shellcheck disable=SC2086
TARE_VERSION="$version" ./tools/build.sh $targets

for t in $targets; do
	os=${t%/*}
	arch=${t#*/}
	stage="dist/stage-$os-$arch"
	mkdir -p "$stage"
	cp LICENSE README.md "$stage/"
	if [ "$os" = windows ]; then
		cp "bin/kubectl-tare-$os-$arch.exe" "$stage/kubectl-tare.exe"
		(cd "$stage" && zip -qX "../kubectl-tare_${os}_${arch}.zip" kubectl-tare.exe LICENSE README.md)
	else
		cp "bin/kubectl-tare-$os-$arch" "$stage/kubectl-tare"
		tar --no-xattrs -C "$stage" -czf "dist/kubectl-tare_${os}_${arch}.tar.gz" kubectl-tare LICENSE README.md
	fi
	rm -rf "$stage"
done

(cd dist && shasum -a 256 kubectl-tare_* > checksums.txt)
ls -lh dist | awk 'NR > 1 {print $NF, $5}'

if [ "$publish" != --publish ]; then
	echo "built $version into dist/; rerun with --publish to release it"
	exit 0
fi

head=$(git rev-parse HEAD)
if [ -z "$(git branch -r --contains "$head")" ]; then
	echo "HEAD $head is not on any remote branch; push it first" >&2
	exit 1
fi
if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
	if [ "$(git rev-parse "$version^{commit}")" != "$head" ]; then
		echo "tag $version exists and is not HEAD" >&2
		exit 1
	fi
else
	git tag -a "$version" -m "$version"
fi
git push origin "$version"
gh release create "$version" dist/* --verify-tag --title "$version" --generate-notes
