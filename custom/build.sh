#!/usr/bin/env bash
# Build the custom OpenList image: upstream + drivers/icloud_photos + drivers/pixcake.
#   custom/build.sh          # test, compile, build image
#   custom/build.sh test     # only go test of the two drivers
# Upgrading = merge the new upstream tag into this branch, set OPENLIST_VERSION, rerun
# (delete public/dist first so the matching frontend is fetched).
#
# OpenList uses a pure-Go sqlite driver on linux/amd64, so the binary is a static
# CGO_ENABLED=0 cross-build and can be compiled on any host with Go; only the last
# step needs Docker. The binary is copied into the official -aio image.
# The frontend is the upstream prebuilt dist plus the labels and tips of our drivers
# (custom/patch-frontend.py), because the panel reads them from its language pack.
set -euo pipefail
OPENLIST_VERSION="${OPENLIST_VERSION:-v4.2.9}"
FRONTEND_VERSION="${FRONTEND_VERSION:-$OPENLIST_VERSION}"
GOARCH="${GOARCH:-amd64}"
IMAGE="${IMAGE:-openlist-custom:${OPENLIST_VERSION}}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${OUT:-$HERE/.out}"
cd "$HERE"
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"

go test ./drivers/icloud_photos/ ./drivers/pixcake/
[ "${1:-}" = test ] && exit 0

if [ ! -f public/dist/index.html ]; then
  mkdir -p public/dist
  curl -fsSL "https://github.com/OpenListTeam/OpenList-Frontend/releases/download/$FRONTEND_VERSION/openlist-frontend-dist-$FRONTEND_VERSION.tar.gz" \
    | tar -xz -C public/dist
fi
python3 -I custom/patch-frontend.py public/dist custom/i18n

COMMIT="$(git rev-parse --short HEAD)"
PKG=github.com/OpenListTeam/OpenList/v4/internal/conf
mkdir -p "$OUT"
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o "$OUT/openlist" -tags=jsoniter -trimpath \
  -ldflags "-s -w -X $PKG.Version=${OPENLIST_VERSION}+custom -X $PKG.WebVersion=$FRONTEND_VERSION -X $PKG.GitCommit=$COMMIT"
cp custom/Dockerfile "$OUT/Dockerfile"

if command -v docker >/dev/null 2>&1; then
  docker build -t "$IMAGE" --build-arg OPENLIST_VERSION="$OPENLIST_VERSION" "$OUT"
  echo "built $IMAGE"
else
  echo "no docker here: copy $OUT to the Docker host and run"
  echo "  docker build -t $IMAGE --build-arg OPENLIST_VERSION=$OPENLIST_VERSION ."
fi
