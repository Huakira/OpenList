#!/usr/bin/env bash
# Build the custom OpenList image: upstream + drivers/icloud_photos + drivers/pixcake.
#   custom/build.sh          # test the two drivers, compile, build image
#   custom/build.sh test     # only go test of the two drivers
# Upgrading = merge the new upstream tag into this branch, set OPENLIST_VERSION, rerun.
# The binary is compiled in a throwaway container (static, musl) and copied into the
# official -aio image, so the Go module cache never lands in an image layer.
set -euo pipefail
OPENLIST_VERSION="${OPENLIST_VERSION:-v4.2.9}"
FRONTEND_VERSION="${FRONTEND_VERSION:-$OPENLIST_VERSION}"
GO_IMAGE="${GO_IMAGE:-golang:1.26-alpine}"
IMAGE="${IMAGE:-openlist-custom:${OPENLIST_VERSION}}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${OUT:-$HERE/.out}"
GOFLAGS_EXTRA="${GOFLAGS_EXTRA:--p=1}" # keep memory low on small hosts

run_go() {
  docker run --rm -v "$HERE":/src -w /src \
    -v openlist-gomod:/go/pkg/mod -v openlist-gocache:/root/.cache/go-build \
    -e GOTOOLCHAIN=auto -e GOFLAGS="$GOFLAGS_EXTRA" -e OUT_DIR=.out \
    -e VERSION="${OPENLIST_VERSION}+custom" -e WEB_VERSION="$FRONTEND_VERSION" -e COMMIT="${COMMIT:-unknown}" \
    "$GO_IMAGE" sh -c "apk add -q --no-cache gcc musl-dev && $1"
}

run_go 'go test ./drivers/icloud_photos/ ./drivers/pixcake/'
[ "${1:-}" = test ] && exit 0

if [ ! -f "$HERE/public/dist/index.html" ]; then
  mkdir -p "$HERE/public/dist"
  curl -fsSL "https://github.com/OpenListTeam/OpenList-Frontend/releases/download/$FRONTEND_VERSION/openlist-frontend-dist-$FRONTEND_VERSION.tar.gz" \
    | tar -xz -C "$HERE/public/dist"
fi

COMMIT="$(git -C "$HERE" rev-parse --short HEAD)"
export COMMIT
mkdir -p "$OUT"
# Single-quoted: $VERSION etc. are expanded by sh inside the container, from the -e flags above.
run_go 'CGO_ENABLED=1 CGO_LDFLAGS=-static go build -o "$OUT_DIR/openlist" -tags=jsoniter \
  -ldflags "-s -w -linkmode external -extldflags \"-static -fpic\" \
  -X github.com/OpenListTeam/OpenList/v4/internal/conf.Version=$VERSION \
  -X github.com/OpenListTeam/OpenList/v4/internal/conf.WebVersion=$WEB_VERSION \
  -X github.com/OpenListTeam/OpenList/v4/internal/conf.GitCommit=$COMMIT" .'

cp "$HERE/custom/Dockerfile" "$OUT/Dockerfile"
docker build -t "$IMAGE" --build-arg OPENLIST_VERSION="$OPENLIST_VERSION" "$OUT"
echo "built $IMAGE"
