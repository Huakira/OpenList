# icloud-photos-openlist

iCloud Photos as an OpenList storage: a thin Go driver (`driver/icloud_photos`) forwards everything to a Python service (`bridge/`, pyicloud) that holds all rules. Plan and decisions: `docs/plan.md`.

## Rules

- This runs against the owner's main Apple ID. Never write to a photo or album the bridge did not create. The whitelist (`BRIDGE_WRITE_WHITELIST`) stays on until the owner turns it off at M5. Do not turn it off, and do not work around it in code or tests.
- Deleting a file never really deletes the photo: it goes to the archive album. A real delete (`purge`) only happens through an explicit `Other` call.
- Business rules go in `bridge/icloud_bridge/ops.py`. iCloud calls go in `pyicloud_backend.py`. The Go driver only forwards.
- No changes to OpenList's own code. The build adds one import line, nothing else.
- pyicloud stays pinned in `bridge/pyproject.toml`. Bump it on purpose and rerun M0.
- Secrets live only in `deploy/.env` on nyc. Never commit them.

## Test

- Bridge: `cd bridge && pip install -e '.[test]' && pytest -q` (fake backend, no iCloud).
- Driver: `build/build.sh test`. Image: `build/build.sh`.
