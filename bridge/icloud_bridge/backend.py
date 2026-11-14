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
        return rec

    def seed_album(self, name: str, parent_id: str | None = None,
                   is_folder: bool = False) -> AlbumRec:
        rec = AlbumRec(id=self._id("L"), name=name, parent_id=parent_id,
                       is_folder=is_folder)
        self.albums[rec.id] = rec
        self.members[rec.id] = []
        self.version += 1
        return rec

    # --- PhotosBackend ---
    def change_token(self) -> str | None:
        return f"v{self.version}"

    def snapshot(self) -> Snapshot:
        self.snapshots += 1
        with self._lock:
            live = [a for a in self.assets.values()]
            smart = {
                "Favorites": [a.id for a in live if a.favorite and not a.deleted],
                "Hidden": [a.id for a in live if a.hidden and not a.deleted],
                "Videos": [a.id for a in live if a.item_type == "movie" and not a.deleted],
            }
            return Snapshot(assets=live, albums=list(self.albums.values()),
                            memberships={k: list(v) for k, v in self.members.items()},
                            smart=smart)

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
        self.albums[album_id] = replace(self.albums[album_id], name=name)

    def delete_album(self, album_id: str) -> None:
        self._maybe_fail("delete_album")
        self.albums.pop(album_id)
        self.members.pop(album_id, None)

    def add_to_album(self, asset_id: str, album_id: str) -> None:
        self._maybe_fail("add_to_album")
        if asset_id not in self.members[album_id]:
            self.members[album_id].append(asset_id)

    def remove_from_album(self, asset_id: str, album_id: str) -> None:
        self._maybe_fail("remove_from_album")
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
