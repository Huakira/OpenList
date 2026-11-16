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
custom/build.sh        # test + compile (+ docker build)
custom/build.sh test   # only go test
```

- The bridge source is `custom/icloud-photos/bridge` (merged from the former
  `icloud-photos-openlist` repo, history kept). The build copies only
  `bridge/icloud_bridge` into `bridgefs/`; `.gitignore` keeps that copy out of
  git. `BRIDGE_SRC` overrides the source directory.
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
   then the 6-digit code Apple sends (type it into the "2FA code" field and
   save again; clear it afterwards). The half-finished sign-in lives inside the
   bridge process, which is why `Drop` does not stop it.
4. The session cookie is stored in `<data_dir>/session`, so after the first
   success no password is needed — including after restarts.

### Write switches (storage form)

| field | default | effect |
|---|---|---|
| Allow changes to existing albums (`write_existing`) | off | Off: only albums and photos created through this storage can be renamed, moved, archived (`BRIDGE_WRITE_WHITELIST=true`). On: your existing albums and photos can be changed too. |
| Read only (`read_only`) | off | Refuse every write: upload, delete, rename, move, copy (`BRIDGE_READ_ONLY=true`). Wins over the option above. |

Whatever the switches say, Live Photos (still + paired video) are always
read-only, and "delete" never deletes: it moves the photo into the archive album.
Changing a switch restarts the bridge process (it is part of the process key).
Uploads are streamed to the bridge's `staging/` directory first, then pushed to
iCloud and added to the target album.

Data written under `<data_dir>`: `session/`, `index.sqlite3`, `audit.jsonl`,
`staging/`, `bridge/`, `bridge.log`.

## PixCake storage

Fill **either** `Token (JWT)` **or** `Token V2` + `Hardware UUID`.

**Getting the JWT** (needs the PixCake macOS client signed in on that Mac):

1. The client keeps its login in a local SQLite database. Read the encrypted
   token (`tokenV2`, hex):
   ```bash
   sqlite3 "$HOME/Library/Application Support/PixCake-qt_pro/db/base.db" \
     "SELECT tokenV2 FROM user ORDER BY login_time DESC LIMIT 1;"
   ```
2. Get the Mac's hardware UUID (the AES key is derived from it):
   ```bash
   system_profiler SPHardwareDataType | grep UUID
   ```
3. Paste both into `Token V2` and `Hardware UUID`; the driver decrypts it itself
   and uses the resulting JWT (about 345 characters, starts with `eyJ`).
   If you already have the plain JWT (e.g. from the client's request headers),
   paste it into `Token (JWT)` instead and leave the other two empty.

The JWT expires; when listing starts failing with an auth error, repeat the
steps above and save the storage again. This is a reverse-engineered client API,
not an official one.

Other fields: `Show team projects` also lists team projects; `Mount a single
project` takes a project ID; `Read only` (default off) refuses create-project,
upload and delete. PixCake has no rename/move/copy API, so those are always
refused.

## Deploy / rollback on nyc

`/opt/openlist/docker-compose.yml` is upstream and never edited;
`/opt/openlist/docker-compose.override.yml` only sets `image:
openlist-custom:v4.2.9`.

```bash
cd /opt/openlist && docker compose up -d        # pick up the rebuilt image
```

Rollback: remove the override file (falls back to `v4.2.7-aio`) and restore
`/opt/openlist/data` from `/opt/backups/openlist-*`.

The storage's `proxy` field is left empty: the bridge connects to iCloud
directly. On nyc, China-bound iCloud addresses are routed to the la exit node
by the host's policy routing (table 100, EasyTier `et-parmac`); other Apple
endpoints leave nyc directly. The old `icloud-ssh-socks` tunnel is no longer
used.
