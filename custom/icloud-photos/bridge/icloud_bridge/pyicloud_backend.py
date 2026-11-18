"""PhotosBackend on top of pyicloud (version pinned in pyproject.toml).

Operations pyicloud lacks (plan 3.2) are written here in the same CloudKit
`records/modify` style pyicloud uses, so they can go upstream later:
remove_from_album, set hidden, recover. All three are UNVERIFIED until M0.
"""

from __future__ import annotations

import logging
import os
import re
import threading
import time
from dataclasses import replace
from pathlib import Path
from typing import Any, Callable, TypeVar
from urllib.parse import urlencode

import requests
from pyicloud import PyiCloudService
from pyicloud.common.cloudkit import CKZoneChangesZoneReq, CKZoneIDReq
from pyicloud.const import CONTENT_TYPE, CONTENT_TYPE_TEXT
from pyicloud.exceptions import PyiCloudServiceUnavailable
from pyicloud.services.photos.exceptions import PhotosApiError, PhotosRateLimited, PhotosServiceException
from pyicloud.services.photos.mappers import (
    decode_encrypted_text,
    record_change_tag,
    record_field_value,
    record_name,
    record_record_type,
    record_zone,
)
from pyicloud.services.photos.service import PhotoAlbumFolder, SmartPhotoAlbum

from .backend import AlbumRec, AssetRec, BackendError, Changes, Known, PhotosBackend, Snapshot

log = logging.getLogger(__name__)
T = TypeVar("T")

# How M0 ④ decides to drop a CPLContainerRelation. Default follows how
# pyicloud deletes albums (flag isDeleted); "forceDelete" removes the record.
RELATION_REMOVE_MODE = os.environ.get("BRIDGE_RELATION_REMOVE_MODE", "isDeleted")
SKIP_SMART = {"Library", "Recently Deleted"}
# A RAW+JPEG shot is one iCloud record: the JPEG is the "original" resource and
# the RAW sits in "alternative". The RAW is listed as its own read-only entry
# whose id is the photo's id plus this suffix.
RAW_SUFFIX = "~raw"
# Id prefix of every shared-stream record (albums and assets), so they can never
# collide with private-zone ids and writes can refuse them in one check.
SHARED_PREFIX = "s:"
ROOT_FOLDER = "----Root-Folder----"
_NOT_RAW = {".jpg", ".jpeg", ".heic", ".heif", ".png", ".mov", ".mp4", ".m4v"}


def _split_raw(asset_id: str, version: str) -> tuple[str, str]:
    """(real asset id, resource version) for an id that may be a RAW entry."""
    if asset_id.endswith(RAW_SUFFIX):
        return asset_id[: -len(RAW_SUFFIX)], "alternative" if version == "original" else version
    return asset_id, version

# iCloud signs download URLs with a short lifetime (the `e` query parameter, a
# unix timestamp; ~15 minutes when observed). The in-memory asset map can live
# for days, so a URL taken from it goes stale long before the snapshot changes.
_URL_EXPIRY = re.compile(r"[?&]e=(\d+)")
# Seconds before expiry at which a record is re-read. Overridable so the refresh
# path can be exercised on demand (a huge value forces it on every download).
URL_REFRESH_MARGIN = int(os.environ.get("BRIDGE_URL_REFRESH_MARGIN", "120"))


def url_ttl(url: str | None) -> int | None:
    """Seconds until a signed URL expires, or None when it carries no expiry."""
    if not url:
        return None
    m = _URL_EXPIRY.search(url)
    return None if m is None else int(m.group(1)) - int(time.time())


_URL_LOCKS: dict[str, threading.Lock] = {}
_URL_LOCKS_GUARD = threading.Lock()


def _asset_lock(asset_id: str) -> threading.Lock:
    """One lock per asset, so two downloads of different assets never queue up."""
    with _URL_LOCKS_GUARD:
        lock = _URL_LOCKS.get(asset_id)
        if lock is None:
            lock = _URL_LOCKS[asset_id] = threading.Lock()
        return lock


def _cn_shared_host(url: str) -> str:
    """The album list answers on the .cn service, but the albumlocation it
    returns still names the international host, and the session cookie
    (X-APPLE-WEBAUTH-USER, domain .icloud.com.cn) does not reach it -> 401.
    Rewrite the shared-streams host (verified on the real account, 2026-11-18)."""
    if "icloud.com.cn" in url:
        return url
    for marker in ("sharedstreams.icloud.com:", "sharedstreams.icloud.com/"):
        if marker in url:
            return url.replace(marker, marker.replace(".com", ".com.cn"))
    return url



def _is_network(e: BaseException) -> bool:
    """A timeout or dropped connection (pyicloud wraps these in PhotosApiError)."""
    cause = e.__cause__ or e.__context__
    return (isinstance(e, requests.RequestException) or isinstance(cause, requests.RequestException)
            or "Request failed to iCloud" in str(e))


def _retry(fn: Callable[[], T], what: str, attempts: int = 6) -> T:
    """Back off on 429, ZONE_BUSY (plan 3.4) and network timeouts."""
    delay = 2.0
    net = 0
    for i in range(attempts):
        try:
            return fn()
        except PhotosRateLimited as e:
            wait = e.retry_after or delay
        except PhotosApiError as e:
            if _is_network(e):
                net += 1
                wait = delay
            elif "ZONE_BUSY" not in str(e) and "ZONE_BUSY" not in str(e.payload):
                raise
            else:
                wait = delay
        except requests.RequestException:
            net += 1
            wait = delay
        if net > 3:
            raise BackendError(f"{what}: iCloud unreachable (network errors)")
        if i == attempts - 1:
            break
        log.warning("%s: iCloud busy or slow, retry in %.0fs", what, wait)
        time.sleep(wait)
        delay = min(delay * 2, 60)
    raise BackendError(f"{what}: iCloud still busy after {attempts} attempts")


def _name_and_type(rec) -> tuple[str | None, str | None]:
    """(recordName, recordType) of a change record. A tombstone (typed
    CKTombstoneRecord or a bare dict) has no type: that is what marks it."""
    if isinstance(rec, dict):
        return rec.get("recordName"), rec.get("recordType")
    return getattr(rec, "recordName", None), getattr(rec, "recordType", None)


def _flag(record: Any, name: str) -> bool:
    """CloudKit flags come as 0/1 ints or "0"/"1" strings."""
    v = record_field_value(record, name)
    try:
        return int(v or 0) != 0
    except (TypeError, ValueError):
        return bool(v)


class PyiCloudBackend(PhotosBackend):
    def __init__(self, apple_id: str, cookie_dir: Path, china: bool = False) -> None:
        cookie_dir.mkdir(parents=True, exist_ok=True)
        self.china = china
        # No password: the session saved by the login step is reused.
        self.api = PyiCloudService(apple_id, cookie_directory=str(cookie_dir),
                                   china_mainland=china)
        if self.api.requires_2fa or self.api.requires_2sa:
            raise BackendError("no iCloud session yet: fill in the Apple ID and password in "
                               "the storage settings, then save again with the verification "
                               "code Apple sends")
        self._photos: dict[str, Any] = {}   # asset id -> PhotoAsset (for URLs, writes)
        self._albums: dict[str, Any] = {}   # album id -> PhotoAlbum
        # Shared streams: separate namespaces, no CloudKit zone, never written.
        # guid -> SharedPhotoStreamAlbum / "s:" id -> PhotoStreamAsset.
        self._shared_objs: dict[str, Any] = {}
        self._shared_photos: dict[str, Any] = {}
        self._shared_album_of: dict[str, str] = {}
        self._load_lock = threading.Lock()

    @property
    def library(self):
        return self.api.photos

    def _load_one(self, asset_id: str):
        """The PhotoAsset behind an index id, fetched on first use (one record
        lookup) so a restarted process needs no library walk to serve files."""
        p = self._photos.get(asset_id)
        if p is not None:
            return p
        with _asset_lock(asset_id):
            p = self._photos.get(asset_id)
            if p is None:
                p = self._fetch(asset_id)
                if p is not None:
                    self._photos[asset_id] = p
        return p

    def _fetch(self, asset_id: str):
        """PhotoAsset for one id from two record lookups (asset, then its master).
        Not library.all.get(): for an id iCloud does not know that falls back to
        walking the whole library, minutes long, which a stale link must not cause."""
        lib = self.library._get_root_library()
        resp = _retry(lambda: lib._client.lookup(
            record_names=[asset_id], zone_id=CKZoneIDReq(**lib.zone_id)), f"lookup {asset_id}")
        asset = next((r for r in getattr(resp, "records", None) or []
                      if _name_and_type(r) == (asset_id, "CPLAsset")), None)
        if asset is None:
            return None
        ref = record_field_value(asset, "masterRef")
        mid = getattr(ref, "recordName", None) or (ref or {}).get("recordName")
        master = self._lookup_master(lib, mid) if mid else None
        if master is None:
            return None
        alb = self.library.all
        return alb._library.asset_type(alb.service, master, asset, library=alb._library)

    # ---------------- shared photo streams ----------------
    def _shared_stream_albums(self) -> list[Any]:
        """SharedPhotoStreamAlbum objects, with the China host rewrite applied.
        One webgetalbumslist call. An account without the shared-streams host
        gets an empty list, not an error."""
        try:
            albums = list(self.library.shared_streams)
        except PyiCloudServiceUnavailable:
            return []
        if self.china:
            for a in albums:
                a._album_location = _cn_shared_host(a._album_location)
        return albums

    def shared_albums(self) -> list[AlbumRec]:
        out: list[AlbumRec] = []
        for a in self._shared_stream_albums():
            self._shared_objs[a.id] = a
            out.append(AlbumRec(id=SHARED_PREFIX + a.id, name=a.name,
                                parent_id=None, is_folder=False))
        return out

    def shared_album_assets(self, album_id: str) -> tuple[AlbumRec, list[AssetRec]]:
        guid = album_id[len(SHARED_PREFIX):]
        alb = self._shared_objs.get(guid)
        if alb is None:      # restarted process, or the list call has not run yet
            alb = next((a for a in self._shared_stream_albums() if a.id == guid), None)
            if alb is None:
                raise BackendError(f"shared album {album_id} not found")
            self._shared_objs[guid] = alb
        photos = _retry(lambda: list(alb.photos), f"shared assets {guid}")
        assets: list[AssetRec] = []
        for p in photos:
            key = SHARED_PREFIX + p.id
            self._shared_photos[key] = p
            self._shared_album_of[key] = guid
            assets.append(replace(self._rec(p), id=key))
        return AlbumRec(id=SHARED_PREFIX + guid, name=alb.name,
                        parent_id=None, is_folder=False), assets

    def _shared_fresh_url(self, asset_id: str, version: str) -> str | None:
        """Signed URL for a shared asset. Shared streams have no per-asset
        lookup (no CloudKit zone), so a stale URL means re-reading the album."""
        p = self._shared_photos.get(asset_id)
        url = p.download_url(version) if p is not None else None
        if url is not None:
            ttl = url_ttl(url)
            if ttl is None or ttl > URL_REFRESH_MARGIN:
                return url
        guid = self._shared_album_of.get(asset_id)
        alb = self._shared_objs.get(guid) if guid else None
        if alb is None:
            return url
        try:
            photos = _retry(lambda: list(alb.photos), f"shared refresh {guid}")
        except BackendError:
            return url                    # keep the stale URL; the caller retries
        for q in photos:
            self._shared_photos[SHARED_PREFIX + q.id] = q
            self._shared_album_of[SHARED_PREFIX + q.id] = guid
        p = self._shared_photos.get(asset_id)
        return (p.download_url(version) if p is not None else None) or url

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
        raws = {a.id: r for a in assets if (r := self._raw_rec(photos[a.id], a))}
        assets += raws.values()
        for table in (members, smart):
            for key, ids in table.items():
                ids.extend(i + RAW_SUFFIX for i in list(ids) if i in raws)
        return Snapshot(assets=assets, albums=albums, memberships=members, smart=smart)

    def change_token(self) -> str | None:
        """Current syncToken of the primary zone (one zones/list call)."""
        lib = self.library._get_root_library()
        lib._current_sync_token = None   # pyicloud caches it; force a fresh read
        try:
            return _retry(lib.sync_cursor, "sync_cursor")
        except BackendError:
            raise                        # busy or unreachable: try again soon, do not re-read everything
        except Exception:
            log.warning("no sync token; falling back to a full snapshot", exc_info=True)
            return None

    # Record types whose changes only a full read can sort out (album layout).
    _FULL_READ_TYPES: set[str] = set()
    MAX_INCREMENTAL = 3000

    def changes(self, since: str, known: Known | None = None) -> Changes | None:
        """Only what changed since `since`, from CloudKit's zone changes feed
        (the same feed iCloud.com uses). None when a full read is safer."""
        lib = self.library._get_root_library()
        req = CKZoneChangesZoneReq(zoneID=lib.zone_id, syncToken=since, reverse=False)
        try:
            zones = _retry(lambda: list(lib._client.iter_changes(zone_req=req)), "changes")
        except BackendError:
            raise
        except Exception:
            log.warning("changes(%s) failed; falling back to a full snapshot", since[:8], exc_info=True)
            return None
        by_master = {p.master_id: pid for pid, p in self._photos.items()}
        assets: dict[str, Any] = {}      # CPLAsset record name (= asset id) -> record
        masters: dict[str, Any] = {}     # CPLMaster record name -> record
        removed: set[str] = set()
        member_add: list[tuple[str, str]] = []
        member_del: list[tuple[str, str]] = []
        albums: dict[str, AlbumRec] = {}
        albums_gone: set[str] = set()
        relations: list[tuple[str, str, bool]] = []
        token = since
        for zone in zones:
            token = zone.syncToken or token
            for rec in zone.records:
                name, rtype = _name_and_type(rec)
                if rtype in self._FULL_READ_TYPES:
                    return None
                if "-IN-" in (name or ""):
                    aid, _, alb_id = name.partition("-IN-")
                    relations.append((alb_id, aid, rtype is None or bool(_flag(rec, "isDeleted"))))
                    continue
                if rtype == "CPLAlbum":
                    if name == ROOT_FOLDER:
                        continue
                    label = decode_encrypted_text(rec, "albumNameEnc")
                    try:
                        kind = int(record_field_value(rec, "albumType"))
                    except Exception:
                        kind = 0
                    if kind not in (0, 3):
                        continue         # smart albums are queries, not stored records
                    if label is None or _flag(rec, "isDeleted"):
                        albums_gone.add(name)
                        continue
                    parent = record_field_value(rec, "parentId")
                    albums[name] = AlbumRec(id=name, name=label,
                                            parent_id=None if parent in (None, "", ROOT_FOLDER) else parent,
                                            is_folder=kind == 3)
                    continue
                if rtype is None or bool(getattr(rec, "deleted", False)) and rtype in ("CPLAsset", "CPLMaster"):
                    if name in self._photos or (known and known.asset(name)):
                        removed.add(name)
                    elif name in by_master:
                        removed.add(by_master[name])
                    elif known is not None and known.album(name):
                        albums_gone.add(name)
                    elif known is None:
                        return None      # nothing to tell us what this was
                    # else: the master of an asset gone in this same batch, or a
                    # relation/record that never reached the index
                elif rtype == "CPLAsset":
                    assets[name] = rec
                elif rtype == "CPLMaster":
                    masters[name] = rec
        wanted = set()                   # masters the batch's assets point at
        for rec in assets.values():
            ref = record_field_value(rec, "masterRef")
            wanted.add(getattr(ref, "recordName", None) or (ref or {}).get("recordName"))
        if any(m not in by_master and m not in wanted for m in masters):
            return None                  # a master alone, for an asset we do not hold
        batch_albums = set(albums)
        for alb_id, aid, gone in relations:
            if alb_id in albums_gone or not (alb_id in batch_albums or (known and known.album(alb_id))):
                continue                 # smart/unknown container, or the album itself is going
            (member_del if gone else member_add).append((alb_id, aid))
        affected = (set(assets) | {by_master[m] for m in masters if m in by_master}) - removed
        if len(affected) + len(removed) > self.MAX_INCREMENTAL:
            return None
        alb = self.library.all
        upserts: list[AssetRec] = []
        fresh: dict[str, Any] = {}
        for aid in sorted(affected):
            old = self._photos.get(aid)
            asset = assets.get(aid) or (old.asset_record if old else None)
            ref = record_field_value(asset, "masterRef") if asset is not None else None
            mid = getattr(ref, "recordName", None) or (ref or {}).get("recordName") \
                or (old.master_id if old else None)
            master = masters.get(mid) or (old.master_record if old else None)
            if master is None and mid and asset is not None:
                master = self._lookup_master(lib, mid)   # e.g. a lone favorite toggle
            if master is None or asset is None:
                return None              # half a record: let a full read sort it out
            p = alb._library.asset_type(alb.service, master, asset, library=alb._library)
            fresh[aid] = p
            rec = self._rec(
                p, hidden=_flag(asset, "isHidden"),
                deleted=_flag(asset, "isDeleted") or _flag(master, "isDeleted"))
            upserts.append(rec)
            if (raw := self._raw_rec(p, rec)) is not None:
                upserts.append(raw)
        self._photos.update(fresh)
        for mid in removed:
            self._photos.pop(mid, None)
        gone = sorted(removed | {i + RAW_SUFFIX for i in removed})
        return Changes(token=token, upserts=upserts, removed=gone,
                       member_add=member_add, member_del=member_del,
                       albums=list(albums.values()), albums_gone=sorted(albums_gone))

    def forget_album(self, album_id: str) -> None:
        """Drop the cached album object (renamed or deleted elsewhere)."""
        self._albums.pop(album_id, None)

    def _lookup_master(self, lib, master_id: str):
        try:
            resp = _retry(lambda: lib._client.lookup(
                record_names=[master_id], zone_id=CKZoneIDReq(**lib.zone_id)), "lookup master")
        except Exception:
            log.warning("lookup of master %s failed", master_id, exc_info=True)
            return None
        for rec in getattr(resp, "records", None) or []:
            if _name_and_type(rec) == (master_id, "CPLMaster"):
                return rec
        return None

    @staticmethod
    def _raw_rec(p, base: AssetRec) -> AssetRec | None:
        """The RAW half of a RAW+JPEG pair, as a read-only entry of its own."""
        if base.item_type != "image" or base.live:
            return None
        try:
            res = p.resources.get("alternative")
        except Exception:
            return None
        name = getattr(res, "filename", None) if res else None
        if not name or Path(name).suffix.lower() in _NOT_RAW:
            return None
        return replace(base, id=base.id + RAW_SUFFIX, filename=name,
                       size=int(getattr(res, "size", 0) or 0))

    @staticmethod
    def _rec(p, *, hidden: bool = False, deleted: bool = False) -> AssetRec:
        return AssetRec(
            id=p.id, filename=p.filename, size=p.size or 0, created=p.asset_date,
            modified=p.added_date, item_type="movie" if p.item_type == "movie" else "image",
            favorite=bool(record_field_value(p.asset_record, "isFavorite")), hidden=hidden, deleted=deleted,
            live=bool(getattr(p, "is_live_photo", False)),
        )

    def download_url(self, asset_id: str, version: str = "original") -> str | None:
        """URL from the cached record. No iCloud round trip: listings call this
        once per asset, so it must stay cheap. See fresh_url() for downloads."""
        asset_id, version = _split_raw(asset_id, version)
        if asset_id.startswith(SHARED_PREFIX):
            p = self._shared_photos.get(asset_id)
            return p.download_url(version) if p is not None else None
        p = self._load_one(asset_id)
        if p is None:
            return None
        return p.download_url(version)

    def fresh_url(self, asset_id: str, version: str = "original") -> str | None:
        """URL that is guaranteed to be valid now, re-reading the record if needed.

        iCloud signs URLs for ~15 minutes while the in-memory record lives for
        days, so the cached URL of a photo opened later in the day has long
        expired and every download would answer 410.
        """
        asset_id, version = _split_raw(asset_id, version)
        if asset_id.startswith(SHARED_PREFIX):
            return self._shared_fresh_url(asset_id, version)
        url = self.download_url(asset_id, version)
        ttl = url_ttl(url)
        if ttl is not None and ttl > URL_REFRESH_MARGIN:
            return url
        fresh = self._requery(asset_id, self._photos.get(asset_id))
        if fresh is None:
            return url
        self._photos[asset_id] = fresh
        return fresh.download_url(version) or url

    def force_requery(self, asset_id: str) -> None:
        """Drop the cached record so the next call re-reads it."""
        if asset_id.startswith(SHARED_PREFIX):
            return          # no per-asset lookup; _shared_fresh_url re-reads the album
        with _asset_lock(asset_id):
            self._photos.pop(asset_id, None)

    def _requery(self, asset_id: str, stale: Any):
        """Re-read one asset record; None when iCloud will not hand it over.

        One asset lookup by record name (a few seconds), not a library walk, so
        it is cheap enough to run per download.
        """
        with _asset_lock(asset_id):
            current = self._photos.get(asset_id)
            if current is not None and current is not stale:
                ttl = url_ttl(current.download_url("original"))
                if ttl is not None and ttl > URL_REFRESH_MARGIN:
                    return current     # another thread already refreshed it
            try:
                rec = self._fetch(asset_id)
            except Exception:
                log.warning("re-query of %s failed; keeping the cached URL", asset_id, exc_info=True)
                return None
            return rec

    def _photo(self, asset_id: str):
        p = self._load_one(asset_id)
        if p is None:
            raise BackendError(f"asset {asset_id} not found in iCloud")
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
        try:
            album = _retry(lambda: self.library.create_album(name), "create_album")
        except PhotosServiceException as e:
            # pyicloud says "outcome is uncertain": on the real account the album was
            # created anyway (M0). Never retry blindly (that could make a duplicate):
            # confirm by exact name, and fail only if it really is absent.
            album = self._find_plain_album(name)
            if album is None:
                raise BackendError(f"create_album outcome uncertain and no album named {name!r} found; not retried") from e
            log.warning("create_album %r: outcome was uncertain, album exists: %s", name, album.id)
        self._albums[album.id] = album
        return AlbumRec(id=album.id, name=name)

    def _find_plain_album(self, name: str):
        """Exactly one ordinary (not smart, not folder) album with this name, else None."""
        hits = [a for a in self.library.albums
                if not isinstance(a, (SmartPhotoAlbum, PhotoAlbumFolder)) and a.name == name]
        return hits[0] if len(hits) == 1 else None

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
