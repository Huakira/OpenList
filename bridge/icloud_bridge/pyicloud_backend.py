"""PhotosBackend on top of pyicloud (version pinned in pyproject.toml).

Operations pyicloud lacks (plan 3.2) are written here in the same CloudKit
`records/modify` style pyicloud uses, so they can go upstream later:
remove_from_album, set hidden, recover. All three are UNVERIFIED until M0.
"""

from __future__ import annotations

import logging
import os
import time
from pathlib import Path
from typing import Any, Callable, TypeVar
from urllib.parse import urlencode

from pyicloud import PyiCloudService
from pyicloud.const import CONTENT_TYPE, CONTENT_TYPE_TEXT
from pyicloud.services.photos.exceptions import PhotosApiError, PhotosRateLimited
from pyicloud.services.photos.mappers import (
    record_change_tag,
    record_field_value,
    record_name,
    record_record_type,
    record_zone,
)
from pyicloud.services.photos.service import PhotoAlbumFolder, SmartPhotoAlbum

from .backend import AlbumRec, AssetRec, BackendError, PhotosBackend, Snapshot

log = logging.getLogger(__name__)
T = TypeVar("T")

# How M0 ④ decides to drop a CPLContainerRelation. Default follows how
# pyicloud deletes albums (flag isDeleted); "forceDelete" removes the record.
RELATION_REMOVE_MODE = os.environ.get("BRIDGE_RELATION_REMOVE_MODE", "isDeleted")
SKIP_SMART = {"Library", "Recently Deleted"}


def _retry(fn: Callable[[], T], what: str, attempts: int = 6) -> T:
    """Back off on 429 and ZONE_BUSY (plan 3.4)."""
    delay = 2.0
    for i in range(attempts):
        try:
            return fn()
        except PhotosRateLimited as e:
            wait = e.retry_after or delay
        except PhotosApiError as e:
            if "ZONE_BUSY" not in str(e) and "ZONE_BUSY" not in str(e.payload):
                raise
            wait = delay
        if i == attempts - 1:
            break
        log.warning("%s: iCloud busy, retry in %.0fs", what, wait)
        time.sleep(wait)
        delay = min(delay * 2, 60)
    raise BackendError(f"{what}: iCloud still busy after {attempts} attempts")


class PyiCloudBackend(PhotosBackend):
    def __init__(self, apple_id: str, cookie_dir: Path) -> None:
        cookie_dir.mkdir(parents=True, exist_ok=True)
        # No password: the session saved by `icloud-bridge login` is reused.
        self.api = PyiCloudService(apple_id, cookie_directory=str(cookie_dir))
        if self.api.requires_2fa or self.api.requires_2sa:
            raise BackendError("iCloud session needs 2FA: run `icloud-bridge login`")
        self._photos: dict[str, Any] = {}   # asset id -> PhotoAsset (for URLs, writes)
        self._albums: dict[str, Any] = {}   # album id -> PhotoAlbum

    @property
    def library(self):
        return self.api.photos

    # ---------------- reads ----------------
    def snapshot(self) -> Snapshot:
        lib = self.library
        photos: dict[str, Any] = {}
        hidden: set[str] = set()
        deleted: set[str] = set()
        albums: list[AlbumRec] = []
        members: dict[str, list[str]] = {}
        smart: dict[str, list[str]] = {}
        self._albums = {}

        for p in lib.all.photos:
            photos[p.id] = p
        for album in lib.albums:
            if isinstance(album, SmartPhotoAlbum):
                if album.name in SKIP_SMART:
                    if album.name == "Recently Deleted":
                        for p in album.photos:
                            photos.setdefault(p.id, p)
                            deleted.add(p.id)
                    continue
                ids = []
                for p in album.photos:
                    photos.setdefault(p.id, p)
                    ids.append(p.id)
                smart[album.name] = ids
                if album.name == "Hidden":
                    hidden.update(ids)
                continue
            self._albums[album.id] = album
            albums.append(AlbumRec(id=album.id, name=album.name,
                                   parent_id=album.parent_id,
                                   is_folder=isinstance(album, PhotoAlbumFolder)))
            if not isinstance(album, PhotoAlbumFolder):
                ids = []
                for p in album.photos:
                    photos.setdefault(p.id, p)
                    ids.append(p.id)
                members[album.id] = ids

        self._photos = photos
        assets = [self._rec(p, hidden=pid in hidden, deleted=pid in deleted)
                  for pid, p in photos.items()]
        return Snapshot(assets=assets, albums=albums, memberships=members, smart=smart)

    def change_token(self) -> str | None:
        """Current syncToken of the primary zone (one zones/list call)."""
        lib = self.library._get_root_library()
        lib._current_sync_token = None   # pyicloud caches it; force a fresh read
        try:
            return _retry(lib.sync_cursor, "sync_cursor")
        except Exception:
            log.warning("no sync token; falling back to a full snapshot", exc_info=True)
            return None

    @staticmethod
    def _rec(p, *, hidden: bool = False, deleted: bool = False) -> AssetRec:
        return AssetRec(
            id=p.id, filename=p.filename, size=p.size or 0, created=p.asset_date,
            modified=p.added_date, item_type="movie" if p.item_type == "movie" else "image",
            favorite=bool(record_field_value(p.asset_record, "isFavorite")), hidden=hidden, deleted=deleted,
            live=bool(getattr(p, "is_live_photo", False)),
        )

    def download_url(self, asset_id: str, version: str = "original") -> str | None:
        p = self._photos.get(asset_id)
        if p is None:
            return None
        # Signed URLs expire (M0 ②); M1 decides whether to re-query here.
        return p.download_url(version)

    def _photo(self, asset_id: str):
        p = self._photos.get(asset_id)
        if p is None:
            raise BackendError(f"asset {asset_id} not in snapshot; refresh first")
        return p

    def _album(self, album_id: str):
        a = self._albums.get(album_id) or self.library.albums.get(album_id)
        if a is None:
            raise BackendError(f"album {album_id} not found")
        return a

    # ---------------- album writes ----------------
    def create_album(self, name: str, parent_id: str | None) -> AlbumRec:
        if parent_id:
            raise BackendError("creating albums inside folders is not implemented yet")
        album = _retry(lambda: self.library.create_album(name), "create_album")
        if album is None:
            raise BackendError("create_album returned nothing")
        self._albums[album.id] = album
        return AlbumRec(id=album.id, name=name)

    def rename_album(self, album_id: str, name: str) -> None:
        _retry(lambda: self._album(album_id).rename(name), "rename_album")

    def delete_album(self, album_id: str) -> None:
        if not _retry(lambda: self._album(album_id).delete(), "delete_album"):
            raise BackendError(f"delete_album {album_id} rejected")
        self._albums.pop(album_id, None)

    def add_to_album(self, asset_id: str, album_id: str) -> None:
        album, photo = self._album(album_id), self._photo(asset_id)
        if not _retry(lambda: album.add_photo(photo), "add_to_album"):
            raise BackendError(f"add {asset_id} to {album_id} rejected")

    def remove_from_album(self, asset_id: str, album_id: str) -> None:
        album, photo = self._album(album_id), self._photo(asset_id)
        relation = f"{photo.asset_id}-IN-{album.id}"
        if RELATION_REMOVE_MODE == "forceDelete":
            op = {"operationType": "forceDelete",
                  "record": {"recordName": relation, "recordType": "CPLContainerRelation"}}
        else:
            op = {"operationType": "forceUpdate",
                  "record": {"recordName": relation, "recordType": "CPLContainerRelation",
                             "fields": {"isDeleted": {"value": 1}}}}
        self._modify([op], album._zone_id, "remove_from_album")

    # ---------------- asset writes ----------------
    def set_flag(self, asset_id: str, flag: str, value: bool) -> None:
        photo = self._photo(asset_id)
        if flag == "favorite":
            _retry(lambda: photo.set_favorite(value), "favorite")
            return
        if flag == "deleted" and value:
            if not _retry(photo.delete, "delete"):
                raise BackendError(f"delete {asset_id} rejected")
            return
        field = {"hidden": "isHidden", "deleted": "isDeleted"}.get(flag)
        if field is None:
            raise BackendError(f"unknown flag {flag}")
        rec = photo.asset_record
        op = {"operationType": "update",
              "record": {"recordName": record_name(rec),
                         "recordType": record_record_type(rec),
                         "recordChangeTag": record_change_tag(rec)
                         or record_change_tag(photo.master_record),
                         "fields": {field: {"value": int(value)}}}}
        zone = record_zone(rec) or {"zoneName": "PrimarySync"}
        self._modify([op], zone, f"set {field}={int(value)}")

    def _modify(self, operations: list[dict], zone: dict, what: str) -> None:
        svc = self.library  # PhotosService: service_endpoint, params, session
        url = f"{svc.service_endpoint}/records/modify?{urlencode(svc.params)}"

        def call():
            resp = svc.session.post(url, json={"atomic": True, "zoneID": zone,
                                               "operations": operations},
                                    headers={CONTENT_TYPE: CONTENT_TYPE_TEXT})
            payload = resp.json()
            errors = [r for r in payload.get("records", []) if r.get("serverErrorCode")]
            if errors:
                code = errors[0].get("serverErrorCode")
                if code == "ZONE_BUSY":
                    raise PhotosApiError(what, payload=payload)
                raise BackendError(f"{what}: {code} {errors[0].get('reason', '')}")
            return payload
        _retry(call, what)

    def upload(self, path: Path, filename: str) -> AssetRec:
        # pyicloud takes the file name from the path, so give it the real one.
        workdir = path.parent / f"{path.name}.d"
        workdir.mkdir(exist_ok=True)
        named = workdir / filename
        if not named.exists():
            os.link(path, named)
        try:
            asset = _retry(lambda: self.library.upload(str(named)), "upload")
        finally:
            named.unlink(missing_ok=True)
            workdir.rmdir()
        if asset is None:
            raise BackendError("upload returned no asset")
        self._photos[asset.id] = asset
        return self._rec(asset)

    def health(self) -> dict:
        return {"session": "needs-2fa" if self.api.requires_2fa else "ok",
                "trusted": bool(self.api.is_trusted_session)}
