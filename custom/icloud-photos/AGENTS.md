# icloud-photos-openlist

iCloud Photos as an OpenList storage: a thin Go driver (`driver/icloud_photos`) forwards everything to a Python service (`bridge/`, pyicloud) that holds all rules. Plan and decisions: `docs/plan.md`.

## Rules

- This runs against the owner's main Apple ID. The write whitelist (`BRIDGE_WRITE_WHITELIST`, OpenList form field 允许修改已有内容) is an ordinary deploy-time switch: the owner lifted the "only touch what the bridge created" rule on 2026-11-18. It still defaults to on; do not silently flip it in code or tests.
- Live Photos are read-only, always: `_need_asset` in `ops.py` refuses every write on them, independent of the whitelist and still after M5. Detection is pyicloud's `is_live_photo`.
- Deleting a file never really deletes the photo: it goes to the archive album. A real delete (`purge`) only happens through an explicit `Other` call.
- Business rules go in `bridge/icloud_bridge/ops.py`. iCloud calls go in `pyicloud_backend.py`. The Go driver only forwards.
- No changes to OpenList's own code. The build adds one import line, nothing else.
- pyicloud stays pinned in `bridge/pyproject.toml`. Bump it on purpose and rerun M0.
- Secrets live only in `deploy/.env` on nyc. Never commit them.

## Test

- Bridge: `cd bridge && pip install -e '.[test]' && pytest -q` (fake backend, no iCloud).
- Driver: `build/build.sh test`. Image: `build/build.sh`.
