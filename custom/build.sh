#!/usr/bin/env bash
# Build the custom OpenList image: upstream + icloud_photos + pixcake + immich.
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
# The iCloud bridge (Python) is copied into drivers/icloud_photos/bridgefs first and
# embedded in the binary, so the storage needs no second service.
set -euo pipefail
OPENLIST_VERSION="${OPENLIST_VERSION:-v4.2.9}"
FRONTEND_VERSION="${FRONTEND_VERSION:-$OPENLIST_VERSION}"
GOARCH="${GOARCH:-amd64}"
IMAGE="${IMAGE:-openlist-custom:${OPENLIST_VERSION}}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${OUT:-$HERE/.out}"
# The bridge source lives in this repo (custom/icloud-photos); only its Python package is copied.
BRIDGE_SRC="${BRIDGE_SRC:-$HERE/custom/icloud-photos}"
cd "$HERE"
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"

if [ -f "$BRIDGE_SRC/bridge/pyproject.toml" ]; then
  rm -rf drivers/icloud_photos/bridgefs/icloud_bridge
  cp -R "$BRIDGE_SRC/bridge/icloud_bridge" drivers/icloud_photos/bridgefs/icloud_bridge
  find drivers/icloud_photos/bridgefs -name __pycache__ -type d -prune -exec rm -rf {} +
  echo "embedded bridge from $BRIDGE_SRC"
elif [ ! -d drivers/icloud_photos/bridgefs/icloud_bridge ]; then
  echo "warning: $BRIDGE_SRC/bridge not found; the binary will have no embedded bridge" >&2
fi

go test ./drivers/icloud_photos/ ./drivers/pixcake/ ./drivers/immich/
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
cp custom/Dockerfile custom/requirements.txt "$OUT/"

if command -v docker >/dev/null 2>&1; then
  docker build -t "$IMAGE" --build-arg OPENLIST_VERSION="$OPENLIST_VERSION" "$OUT"
  echo "built $IMAGE"
else
  echo "no docker here: copy $OUT to the Docker host and run"
  echo "  docker build -t $IMAGE --build-arg OPENLIST_VERSION=$OPENLIST_VERSION ."
fi
