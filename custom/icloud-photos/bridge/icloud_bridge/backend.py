"""Backend interface between the bridge and iCloud.

`PhotosBackend` is the only thing that talks to iCloud. Business rules live in
ops.py and never call pyicloud directly, so they can be tested with
`FakeBackend`.
"""

from __future__ import annotations

import hashlib
import itertools
import threading
from abc import ABC, abstractmethod
from typing import Callable
from dataclasses import dataclass, field, replace
from datetime import datetime, timezone
from pathlib import Path


@dataclass(frozen=True)
class AssetRec:
    id: str
    filename: str
    size: int
    created: datetime
    modified: datetime
    item_type: str = "image"  # image | movie
    favorite: bool = False
    hidden: bool = False
    deleted: bool = False
    live: bool = False        # Live Photo: always read-only, whatever the whitelist says


@dataclass(frozen=True)
class AlbumRec:
    id: str
    name: str
    parent_id: str | None = None
    is_folder: bool = False


@dataclass
class Snapshot:
    assets: list[AssetRec]
    albums: list[AlbumRec]
    memberships: dict[str, list[str]]          # album_id -> asset ids
    smart: dict[str, list[str]] = field(default_factory=dict)  # smart name -> ids


@dataclass
class Changes:
    """What moved in the library since a sync token (CloudKit zone changes)."""
    token: str
    upserts: list[AssetRec] = field(default_factory=list)   # new or changed assets
    removed: list[str] = field(default_factory=list)        # asset ids gone for good
    member_add: list[tuple[str, str]] = field(default_factory=list)   # (album id, asset id)
    member_del: list[tuple[str, str]] = field(default_factory=list)
    albums: list[AlbumRec] = field(default_factory=list)    # new or renamed albums/folders
    albums_gone: list[str] = field(default_factory=list)


@dataclass
class Known:
    """What the index already holds: lets changes() place a tombstone."""
    asset: Callable[[str], bool]
    album: Callable[[str], bool]


class PhotosBackend(ABC):
    """Every method is one iCloud round trip (or a few). No business rules here."""

    @abstractmethod
    def snapshot(self) -> Snapshot: ...

    @abstractmethod
    def download_url(self, asset_id: str, version: str = "original") -> str | None: ...

    @abstractmethod
    def create_album(self, name: str, parent_id: str | None) -> AlbumRec: ...

    @abstractmethod
    def rename_album(self, album_id: str, name: str) -> None: ...

    @abstractmethod
    def delete_album(self, album_id: str) -> None: ...

    @abstractmethod
    def add_to_album(self, asset_id: str, album_id: str) -> None: ...

    @abstractmethod
    def remove_from_album(self, asset_id: str, album_id: str) -> None: ...

    @abstractmethod
    def upload(self, path: Path, filename: str) -> AssetRec: ...

    @abstractmethod
    def set_flag(self, asset_id: str, flag: str, value: bool) -> None:
        """flag in {"favorite", "hidden", "deleted"}; deleted=False is recover."""

    def change_token(self) -> str | None:
        """Cheap token that changes whenever the library changes; None = unknown."""
        return None

    def shared_albums(self) -> list[AlbumRec]:
        """Shared photo stream albums (legacy 共享相册). One webgetalbumslist
        call. Album ids carry the `s:` prefix; parent_id is None (the caller
        hangs them under the shared root). Raises BackendError on failure."""
        raise BackendError("shared albums are not supported by this backend")

    def shared_album_assets(self, album_id: str) -> tuple[AlbumRec, list[AssetRec]]:
        """All assets of one shared album, re-read from iCloud (one paged walk).
        Asset ids carry the `s:` prefix. Raises BackendError on failure."""
        raise BackendError("shared albums are not supported by this backend")

    def changes(self, since: str, known: Known | None = None) -> Changes | None:
        """Incremental update since `since`. None = cannot say (album changes,
        too many changes, unsupported): the caller falls back to snapshot()."""
        return None

    def health(self) -> dict:
        return {"session": "ok"}


class BackendError(Exception):
    """iCloud refused or failed an operation."""


class FakeBackend(PhotosBackend):
    """In-memory library used by tests and local development."""

    def __init__(self, files_dir: Path | None = None) -> None:
        self._lock = threading.Lock()
        self._ids = itertools.count(1)
        self.assets: dict[str, AssetRec] = {}
        self.albums: dict[str, AlbumRec] = {}
        self.members: dict[str, list[str]] = {}
        self.blobs: dict[str, bytes] = {}
        self.calls: list[tuple] = []
        self.fail_on: set[str] = set()   # method names that raise, for failure tests
        self.files_dir = files_dir
        self.version = 0                 # bumps on every change, like a sync token
        self.snapshots = 0
        self.incremental = False         # True: changes() answers instead of None
        self.log: list[tuple[int, str, str]] = []   # (version, kind, id) kind: asset|removed|album
        self.change_calls = 0
        self.shared_fail: set[str] = set()   # "list" or an "s:" album id: raise when read

    def _id(self, prefix: str) -> str:
        return f"{prefix}{next(self._ids):04d}"

    def _maybe_fail(self, name: str) -> None:
        self.calls.append((name,))
        self.version += 1
        if name in self.fail_on:
            raise BackendError(f"injected failure in {name}")

    # --- seeding helpers for tests ---
    def seed_asset(self, filename: str, data: bytes = b"x", **kw) -> AssetRec:
        now = datetime(2001, 1, 1, tzinfo=timezone.utc)
        rec = AssetRec(id=self._id("A"), filename=filename, size=len(data),
                       created=now, modified=now, **kw)
        self.assets[rec.id] = rec
        self.blobs[rec.id] = data
        self.version += 1
        self.log.append((self.version, "asset", rec.id))
        return rec

    def seed_shared_album(self, name: str) -> AlbumRec:
        """A shared photo stream album (id carries the s: prefix)."""
        rec = AlbumRec(id=f"s:{self._id('S')}", name=name, parent_id=None)
        self.albums[rec.id] = rec
        self.members[rec.id] = []
        return rec

    def seed_shared_asset(self, album_id: str, filename: str,
                          data: bytes = b"x", **kw) -> AssetRec:
        """A photo inside a shared album (id carries the s: prefix)."""
        now = datetime(2002, 1, 1, tzinfo=timezone.utc)
        rec = AssetRec(id=f"s:{self._id('A')}", filename=filename, size=len(data),
                       created=now, modified=now, **kw)
        self.assets[rec.id] = rec
        self.blobs[rec.id] = data
        self.members.setdefault(album_id, []).append(rec.id)
        return rec

    def seed_album(self, name: str, parent_id: str | None = None,
                   is_folder: bool = False) -> AlbumRec:
        rec = AlbumRec(id=self._id("L"), name=name, parent_id=parent_id,
                       is_folder=is_folder)
        self.albums[rec.id] = rec
        self.members[rec.id] = []
        self.version += 1
        self.log.append((self.version, "album", rec.id))
        return rec

    def remove_asset(self, asset_id: str) -> None:
        """Test helper: the asset vanishes from iCloud for good."""
        self.assets.pop(asset_id, None)
        self.version += 1
        self.log.append((self.version, "removed", asset_id))

    # --- PhotosBackend ---
    def change_token(self) -> str | None:
        return f"v{self.version}"

    def changes(self, since: str, known: Known | None = None) -> Changes | None:
        self.change_calls += 1
        if not self.incremental or not since.startswith("v"):
            return None
        start = int(since[1:])
        entries = [e for e in self.log if e[0] > start]
        if any(kind == "album" for _, kind, _ in entries):
            return None
        out = Changes(token=f"v{self.version}")
        for _, kind, aid in entries:
            if kind == "removed":
                out.removed.append(aid)
            elif aid in self.assets and all(u.id != aid for u in out.upserts):
                out.upserts.append(self.assets[aid])
        return out

    def snapshot(self) -> Snapshot:
        self.snapshots += 1
        with self._lock:
            # Shared-stream records never ride the private snapshot: they have
            # no change feed and are refreshed on demand (see ops._kick).
            live = [a for a in self.assets.values() if not a.id.startswith("s:")]
            smart = {
                "Favorites": [a.id for a in live if a.favorite and not a.deleted],
                "Hidden": [a.id for a in live if a.hidden and not a.deleted],
                "Videos": [a.id for a in live if a.item_type == "movie" and not a.deleted],
            }
            albums = [l for l in self.albums.values() if not l.id.startswith("s:")]
            members = {k: v for k, v in self.members.items() if not k.startswith("s:")}
            return Snapshot(assets=live, albums=albums, memberships=members, smart=smart)

    def shared_albums(self) -> list[AlbumRec]:
        if "list" in self.shared_fail:
            raise BackendError("injected failure in shared_albums")
        return [l for l in self.albums.values() if l.id.startswith("s:")]

    def shared_album_assets(self, album_id: str) -> tuple[AlbumRec, list[AssetRec]]:
        if album_id in self.shared_fail:
            raise BackendError(f"injected failure in shared_album_assets({album_id})")
        alb = self.albums.get(album_id)
        if alb is None:
            raise BackendError(f"shared album {album_id} not found")
        return alb, [self.assets[i] for i in self.members.get(album_id, [])]

    def download_url(self, asset_id: str, version: str = "original") -> str | None:
        if asset_id not in self.assets:
            return None
        digest = hashlib.sha1(asset_id.encode()).hexdigest()[:8]
        return f"https://cvws.icloud-content.com/fake/{asset_id}/{version}?sig={digest}"

    def create_album(self, name: str, parent_id: str | None) -> AlbumRec:
        self._maybe_fail("create_album")
        return self.seed_album(name, parent_id)

    def rename_album(self, album_id: str, name: str) -> None:
        self._maybe_fail("rename_album")
        self.log.append((self.version, "album", ""))
        self.albums[album_id] = replace(self.albums[album_id], name=name)

    def delete_album(self, album_id: str) -> None:
        self._maybe_fail("delete_album")
        self.log.append((self.version, "album", ""))
        self.albums.pop(album_id)
        self.members.pop(album_id, None)

    def add_to_album(self, asset_id: str, album_id: str) -> None:
        self._maybe_fail("add_to_album")
        self.log.append((self.version, "album", ""))
        if asset_id not in self.members[album_id]:
            self.members[album_id].append(asset_id)

    def remove_from_album(self, asset_id: str, album_id: str) -> None:
        self._maybe_fail("remove_from_album")
        self.log.append((self.version, "album", ""))
        if asset_id in self.members[album_id]:
            self.members[album_id].remove(asset_id)

    def upload(self, path: Path, filename: str) -> AssetRec:
        self._maybe_fail("upload")
        data = Path(path).read_bytes()
        return self.seed_asset(filename, data)

    def set_flag(self, asset_id: str, flag: str, value: bool) -> None:
        self._maybe_fail("set_flag")
        if flag not in {"favorite", "hidden", "deleted"}:
            raise BackendError(f"unknown flag {flag}")
        self.assets[asset_id] = replace(self.assets[asset_id], **{flag: value})
        self.log.append((self.version, "asset", asset_id))
