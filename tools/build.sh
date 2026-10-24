#!/bin/sh
# Builds kubectl-tare into bin/, stamped with a version.
#
# The version matters more here than it does for most tools: these binaries get
# passed to people by hand, and when somebody reports that a recommendation was
# wrong the first question is which build produced it. A release passes its tag
# in TARE_VERSION (tools/release.sh does); otherwise the stamp is the date plus
# a hash of the sources, which is enough to tell two builds apart and to match
# a binary back to a tree.
#
#   ./tools/build.sh                     this machine
#   ./tools/build.sh linux/amd64         one other platform
#   ./tools/build.sh linux/amd64 darwin/arm64 darwin/amd64
set -eu

cd "$(dirname "$0")/.."
mkdir -p bin

sources=$(find cmd internal -name '*.go' ! -name '*_test.go' | sort | xargs shasum | shasum | cut -c1-8)
version=${TARE_VERSION:-"v0.0.0+$(date -u +%Y%m%d).$sources"}

build() {
	goos=${1%/*}
	goarch=${1#*/}
	out="bin/kubectl-tare"
	if [ "$1" != "native" ]; then
		out="bin/kubectl-tare-$goos-$goarch"
		[ "$goos" = windows ] && out="$out.exe"
		export GOOS="$goos" GOARCH="$goarch"
	fi
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out" ./cmd/kubectl-tare
	unset GOOS GOARCH 2>/dev/null || true
	if [ "$1" = "native" ]; then
		ln -sf kubectl-tare bin/kubectl_complete-tare
	fi
	ls -lh "$out" | awk '{print $NF, $5}'
}

if [ $# -eq 0 ]; then
	build native
else
	for target in "$@"; do build "$target"; done
fi

echo "version: $version"
echo "install: sudo install bin/kubectl-tare /usr/local/bin/kubectl-tare && sudo ln -sf kubectl-tare /usr/local/bin/kubectl_complete-tare"
