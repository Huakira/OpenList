# Plan and decisions

The original plan (sections 0 to 9) was written in the DSH session on 2026-11-14. This file keeps only what was decided and how far each part has got.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Delete in 全部照片 / smart albums | Only add the photo to the archive. Do not hide it. | Owner, 2026-11-14. The photo stays in the timeline. |
| Delete a whole album | Archive every photo in it first, then delete the album. | Owner, 2026-11-14. |
| Folder delete | Only when empty. | A folder holds albums, not photos. |
| Rename a photo | Refused. | Photos has no editable filename. |
| Move out of 归档 | Refused, use copy. | Leaving the archive would hide the trail. |
| API addressing | By `id` (`root`, `all`, `deleted`, `smart:<n>`, `album:<id>`, `folder:<id>`, file `f\|<dir>\|<asset>`), not by path. | OpenList hands the driver objects with IDs. Remove needs the parent, and it is encoded in the file id. |
| Whitelist name rule | While the whitelist is on, album names must start with `__bridge_test_`. | Plan 6.4, enforced in code. |
| Overwrite in 全部照片 | The new photo takes the name. The old one stays in 全部照片 (archived) under a `~xxxx` name. | Deleting from All Photos never removes the photo. |
| Deployment | The bridge joins the existing `/opt/openlist` network (`openlist_default`). M4 swaps that OpenList's image for `openlist-icloud:<ver>`. Changed from the plan's "both in one compose". | One OpenList on nyc already holds storages and users. A second one would split them. **Confirm at M4.** |
| Repo | Private `Huakira/icloud-photos-openlist`, cloned at `/opt/icloud-photos`. | Owner, 2026-11-14. |
| Build | `golang:*-alpine` static build, binary swapped into the official `-aio` image. | The official image is alpine/musl. |

## Unverified until M0 (needs login + 2FA)

- `remove_from_album`: CPLContainerRelation `forceUpdate isDeleted=1` vs `forceDelete` (`BRIDGE_RELATION_REMOVE_MODE`). `scripts/m0_probe.py` tries both.
- `isHidden` write, recover (`isDeleted=0`).
- Whether signed URLs work without cookies (302 vs proxy fallback), how long they live, and how long an upload takes to become visible.
- Whether the account has Advanced Data Protection.
- `create_album` inside a folder is not implemented (pyicloud has no parent argument).

## Status

| Milestone | State |
|---|---|
| Code skeleton | Done. Bridge: 12 tests on the fake backend. Driver: `go test` passes inside OpenList v4.2.7. Image builds. |
| M0 prep | Test media: JPEG, PNG, HEIC, MOV generated (`scripts/gen_test_media.py`, 2001-01-01, unique marker). A Live Photo cannot be faked: the HEIC ContentIdentifier sits in Apple MakerNotes, which exiftool cannot create, so the owner must supply one real iPhone Live Photo. Backup: `scripts/backup.py` (preflight, pyicloud sync, count check). |
| M0 | Blocked on: (1) owner login + 2FA; (2) a backup destination. nyc `/` had only 3.2G free on 2026-11-14 (95% used). |
| M1 to M5 | Not started. |

## Known gaps in the skeleton

- Sync is a full snapshot every `BRIDGE_SYNC_INTERVAL`. It is slow on a big library; the plan's incremental sync (`iter_changes`) comes in M1.
- Thumbnail URLs come from the snapshot and may expire between refreshes (see M0 ②).
- Session-expiry notification channel: not decided.
- Shared library: read-only, not wired yet.
