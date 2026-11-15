# bridgefs — the iCloud bridge that ships inside the OpenList binary

`drivers/icloud_photos` embeds this directory with `//go:embed` and, on Init,
extracts it into `data/icloud-photos/bridge` and runs it with the local
`python3`. The storage therefore needs no second service: no address, no token,
just the Apple ID.

The Python package is not in this repository (its owner keeps it in a private
repo). `custom/build.sh` copies `$BRIDGE_SRC/bridge/icloud_bridge` here before
`go build`, so a build without that source produces a binary whose iCloud
storage fails with "二进制里没有内置 bridge（bridgefs 为空）".

Only *third-party* dependencies are installed into the image
(`custom/requirements.txt`); the bridge code itself always comes from the
binary, so the two can never drift apart.
