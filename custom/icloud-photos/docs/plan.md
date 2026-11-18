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
| Shared albums (共享相册) | Read-only: every id carries `s:`, all writes refused by a hard guard (not the whitelist). Root gains a `shared` directory; each scope (album list, one album) refreshes on demand via `_kick` — shared streams have no change feed. China accounts rewrite the `albumlocation` host to `.icloud.com.cn` in `pyicloud_backend.py`. Not to be confused with the CloudKit shared photo library below. | Owner, 2026-11-18. |
| Shared album upload | Not pursued (researched 2026-11-18). Apple's clients can upload to shared albums (icloud.com web, iCloud for Windows, public-album contributors) but no upload endpoint has ever been reverse-engineered: pyicloud builds the stream library with `upload_url=None`; steilerDev's 2026-10 API map and icloudpd #1019 (Jul 2026) document read endpoints only (`webgetalbumslist`/`getchanges`/`webgetalbumview`/`webgetassetcount`/`webgetassets`). Building it means fresh HAR capture of icloud.com/photos. Note (2026-11-18): since iOS 27 albums created or upgraded on 27+ store true originals (counting against the owner's storage); only albums created on <=26 stay transcoded to ~2048px, so lossiness is per-album, not inherent. Upload rate limits under the new policy are unverified. Bridge stays read-only for `s:`. | Owner, 2026-11-18. |
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
| Code skeleton | Done. Bridge: 18 tests on the fake backend. Driver: `go test` passes inside OpenList v4.2.7. Images build. |
| Login | Done 2026-11-14: China mainland Apple ID (`ICLOUD_CHINA=1`), trusted session in `data/session`. |
| Egress | nyc to `icloud.com.cn` is lossy (CMI route, ~1 in 12 TCP connects succeed). The bridge sends all iCloud traffic through the `ssh-socks` sidecar (SSH SOCKS5 to hkdis, dedicated key `data/tunnel/id_ed25519`, `restrict,port-forwarding` on hkdis). 5/5 connects ok via the tunnel. |
| Downloads | No 302. Driver always proxies: OpenList -> bridge `/v1/raw` -> iCloud (Range supported). Thumbnails are not exposed (they would be direct iCloud URLs). Checked end to end on a throwaway OpenList: driver listed, `only_proxy`, 200 with no redirect, bytes identical, Range 206. |
| M0 (real account) | Run 2026-11-14, see below. |
| M1 to M5 | Not run. Whitelist stays on. |

### M0 results (China site, via the tunnel)

- 3 Upload visible after 8.4 s (pyicloud waits for hydration before returning).
- 1 Signed download URL works without cookies (`cvws.icloud-content.com.cn`, plain GET 206). The bridge relies on this.
- 2 URL lifetime: not measured (`m0_probe.py --expiry`).
- 4 Remove from album: `isDeleted` and `forceDelete` both work. Default stays `isDeleted`.
- 5 Hide, unhide, delete (to Recently Deleted) and recover all work. A write with a stale cached record still succeeded.
- ADP: Photos readable through the web API, so Advanced Data Protection is almost certainly off (inferred).
- `create_album` raises "outcome is uncertain" although the album is created. The bridge now confirms by exact name and never retries blindly.
- iCloud ignores the EXIF capture date on upload: the test photo got today's date, not 2001.
- Library size seen: 4432 photos, 37 albums, 1618 Live Photos. A full snapshot takes ~4 min.

## Known gaps in the skeleton

- Refresh first reads the zone syncToken (one call) and takes a full snapshot only when it moved. Applying individual changes (`iter_changes`) is not done: it cannot be tested without the account.
- Thumbnail URLs come from the snapshot and may expire between refreshes (see M0 ②).
- Session-expiry notification channel: not decided.
- Shared library: read-only, not wired yet.
