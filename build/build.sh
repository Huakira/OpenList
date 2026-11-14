#!/usr/bin/env bash
# Build OpenList with the icloud_photos driver added.
#   build/build.sh                 # test + image
#   build/build.sh test            # only go test of the driver
# Upgrading OpenList = change OPENLIST_VERSION and rerun.
set -euo pipefail
OPENLIST_VERSION="${OPENLIST_VERSION:-v4.2.7}"
# Web UI bundled into the binary; official builds use the same tag.
FRONTEND_VERSION="${FRONTEND_VERSION:-$OPENLIST_VERSION}"
GO_IMAGE="${GO_IMAGE:-golang:1.26-alpine}"
IMAGE="${IMAGE:-openlist-icloud:${OPENLIST_VERSION}}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-$HERE/.build/openlist-$OPENLIST_VERSION}"

if [ ! -d "$WORK/.git" ]; then
  mkdir -p "$(dirname "$WORK")"
  git clone -q --depth 1 --branch "$OPENLIST_VERSION" https://github.com/OpenListTeam/OpenList.git "$WORK"
fi
git -C "$WORK" checkout -q -- drivers/all.go
rm -rf "$WORK/drivers/icloud_photos"
cp -r "$HERE/driver/icloud_photos" "$WORK/drivers/icloud_photos"
if [ ! -f "$WORK/public/dist/index.html" ]; then
  rm -rf "$WORK/public/dist" && mkdir -p "$WORK/public/dist"
  curl -fsSL "https://github.com/OpenListTeam/OpenList-Frontend/releases/download/$FRONTEND_VERSION/openlist-frontend-dist-$FRONTEND_VERSION.tar.gz" \
    | tar -xz -C "$WORK/public/dist"
fi
# register: one added import line, nothing else in OpenList changes
sed -i 's#^import (#import (\n\t_ "github.com/OpenListTeam/OpenList/v4/drivers/icloud_photos"#' "$WORK/drivers/all.go"

docker run --rm -v "$WORK":/src -w /src -v openlist-gomod:/go/pkg/mod -e GOTOOLCHAIN=auto \
  -v openlist-gocache:/root/.cache/go-build \
  "$GO_IMAGE" sh -c 'apk add -q --no-cache gcc musl-dev && go test ./drivers/icloud_photos/'
[ "${1:-}" = test ] && exit 0

docker build -t "$IMAGE" --build-arg OPENLIST_VERSION="$OPENLIST_VERSION" \
  --build-arg GO_IMAGE="$GO_IMAGE" \
  --build-arg WEB_VERSION="$FRONTEND_VERSION" --build-arg COMMIT="$(git -C "$WORK" rev-parse --short HEAD)" -f "$HERE/build/Dockerfile" "$WORK"
echo "built $IMAGE"
