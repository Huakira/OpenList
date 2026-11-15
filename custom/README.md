# custom/ — the OpenList build used on nyc

Upstream OpenList `v4.2.9` plus two drivers and a self-contained iCloud bridge.
Everything here is additive: the only change to upstream code is one import line
per driver in `drivers/all.go`.

## What is in the image

| piece | where |
|---|---|
| OpenList binary | upstream `v4.2.9`, `CGO_ENABLED=0`, version stamped `v4.2.9+custom` |
| `drivers/icloud_photos` | Go driver; embeds the Python bridge (`bridgefs/`) and starts it on Init |
| `drivers/pixcake` | PixCake cloud projects (reverse-engineered client API) |
| frontend | upstream prebuilt dist + our labels/tips (`patch-frontend.py`) |
| python3 + pyicloud + fastapi + uvicorn + pysocks | image layer, see `requirements.txt` |

The frontend patch exists because the admin panel does not read a driver's Go
`help` tag: it looks field labels and tips up in its language pack
(`drivers.<driver>.<field>[-tips]`, `drivers.config.<driver>.alert`,
`drivers.drivers.<driver>`). `patch-frontend.py` writes our entries into the
`en` and `zh-CN` chunks; other languages fall back to English.

## Build

```bash
BRIDGE_SRC=/opt/icloud-photos custom/build.sh        # test + compile (+ docker build)
BRIDGE_SRC=/opt/icloud-photos custom/build.sh test   # only go test
```

- `BRIDGE_SRC` is the private bridge checkout; only `bridge/icloud_bridge` is
  copied in, and `.gitignore` keeps it out of this repo.
- The binary is a static cross-build, so it can be compiled on a machine with
  Go; the last step (`docker build`) needs Docker. On nyc (no Go, little disk)
  the binary is built elsewhere and copied with `Dockerfile` + `requirements.txt`
  into a directory, then `docker build -t openlist-custom:$V --build-arg
  OPENLIST_VERSION=$V .` runs there.
- `public/dist` is fetched from the OpenList-Frontend release matching the tag
  and patched in place; delete it before changing `OPENLIST_VERSION`.

## Upgrade to a new upstream tag

```bash
git fetch upstream && git checkout -B nyc-custom upstream/vX.Y.Z   # keep our commits on top
# resolve drivers/all.go if upstream moved it, then
rm -rf public/dist && FRONTEND_VERSION=vX.Y.Z custom/build.sh
```

Watch `internal/driver` and `internal/model` changes: the drivers compile
against them. `go test ./drivers/...` catches most breakage.

## iCloud storage: how it runs

1. `Init` extracts the embedded Python package to `<data_dir>/bridge` (default
   `data/icloud-photos/bridge`), picks a free loopback port, starts
   `python3 -m icloud_bridge serve` and waits for `/v1/health`.
2. One process per data directory, kept in memory across `Init` calls and
   killed when OpenList exits (`Pdeathsig`).
3. Sign-in happens from the storage form, in two saves: Apple ID + password,
   then the 6-digit code Apple sends. The half-finished sign-in lives inside the
   bridge process, which is why `Drop` does not stop it.
4. The session cookie is stored in `<data_dir>/session`, so after the first
   success no password is needed — including after restarts.

Data written under `<data_dir>`: `session/`, `index.sqlite3`, `audit.jsonl`,
`staging/`, `bridge/`, `bridge.log`.

## Deploy / rollback on nyc

`/opt/openlist/docker-compose.yml` is upstream and never edited;
`/opt/openlist/docker-compose.override.yml` only sets `image:
openlist-custom:v4.2.9`.

```bash
cd /opt/openlist && docker compose up -d        # pick up the rebuilt image
```

Rollback: remove the override file (falls back to `v4.2.7-aio`) and restore
`/opt/openlist/data` from `/opt/backups/openlist-*`.

The iCloud bridge talks to iCloud through the `icloud-ssh-socks` container
(`openlist_default` network) because nyc's route to iCloud is unreliable; that
is the storage's `proxy` field, `socks5h://icloud-ssh-socks:1080`.
