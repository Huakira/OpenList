"""All business rules (plan section 2) and all write safety checks (section 6).

Directory keys          File ids
  root                    f|<dir key>|<asset id>
  all
  deleted
  shared                 (shared photo streams: read-only, refreshed on demand)
  smart:<name>
  album:<id>   (the archive album is an ordinary album:<id>, special-cased)
  album:s:<guid>   (a shared album)
  folder:<id>
"""

from __future__ import annotations

import logging
import shutil
import threading
import time
import uuid
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, replace
from datetime import datetime, timezone
from pathlib import Path
from typing import BinaryIO, Callable

from . import naming
from .audit import Audit
from .backend import AlbumRec, AssetRec, BackendError, Known, PhotosBackend
from .config import Settings
from .index import Index
from .thumbs import ThumbCache

log = logging.getLogger(__name__)

SMART_LABELS = {
    "Favorites": "收藏", "Videos": "视频", "Screenshots": "截屏", "Hidden": "已隐藏",
    "Live": "实况", "Panoramas": "全景", "Bursts": "连拍", "Slo-mo": "慢动作",
    "Time-lapse": "延时摄影",
}
OTHER_OPS = {"favorite", "unfavorite", "hide", "unhide", "recover", "purge"}

# Shared photo streams (legacy 共享相册): every album and asset id carries this
# prefix, all writes to them are refused, and they refresh on demand because
# shared streams have no change feed to ride.
SHARED_PREFIX = "s:"
SHARED_DIR = "shared"        # root-level directory key
SHARED_PARENT = "shared"     # sentinel parent_id of shared albums in the index


class OpError(Exception):
    status = 400


class Forbidden(OpError):
    status = 403


class NotFound(OpError):
    status = 404


class Conflict(OpError):
    status = 409


class ReadOnly(OpError):
    status = 423


@dataclass
class Entry:
    id: str
    name: str
    is_dir: bool
    size: int = 0
    modified: str = ""
    created: str = ""
    thumb: str = ""
    status: str = "ready"

    def as_dict(self) -> dict:
        return self.__dict__.copy()


def file_id(dir_key: str, asset_id: str) -> str:
    return f"f|{dir_key}|{asset_id}"


def parse_file_id(fid: str) -> tuple[str, str]:
    parts = fid.split("|")
    if len(parts) != 3 or parts[0] != "f":
        raise NotFound(f"bad file id {fid!r}")
    return parts[1], parts[2]


class Ops:
    def __init__(self, settings: Settings, backend: PhotosBackend, index: Index,
                 audit: Audit) -> None:
        self.s = settings
        self.backend = backend
        self.index = index
        self.audit = audit
        self._wlock = threading.Lock()
        self._fresh_lock = threading.Lock()
        self._inflight: threading.Event | None = None
        self._last_try = float("-inf")          # one write at a time (3.4)
        self._shared_lock = threading.Lock()
        self._shared_inflight: dict[str, threading.Event] = {}
        self._shared_tries: dict[str, float] = {}
        self._uploads = ThreadPoolExecutor(max_workers=1, thread_name_prefix="upload")
        self._pending: list[Future] = []
        self.s.staging_dir.mkdir(parents=True, exist_ok=True)
        self.thumbs = ThumbCache(self.s.thumbs_dir, self.s.thumb_cache_bytes)

    # ======================= sync =======================
    def freshen(self) -> None:
        """Called when someone opens a directory: make sure the index is not
        older than settings.fresh_max_age, instead of polling around the clock.
        Waits at most settings.fresh_wait seconds; a slower refresh (a full
        re-read) keeps running in the background and the stale index is served."""
        with self._fresh_lock:
            ev = self._inflight
            if ev is None:
                if time.monotonic() - self._last_try < self.s.fresh_max_age:
                    return
                self._last_try = time.monotonic()
                ev = self._inflight = threading.Event()

                def run() -> None:
                    try:
                        self.refresh()
                        self.on_refresh(None)
                    except Exception as e:
                        log.warning("refresh failed: %s", e)
                        self.on_refresh(e)
                    finally:
                        with self._fresh_lock:
                            self._inflight = None
                            self._last_try = time.monotonic()
                        ev.set()
                threading.Thread(target=run, daemon=True).start()
        ev.wait(self.s.fresh_wait)

    def on_refresh(self, err: Exception | None) -> None:
        """Hook for the API layer to surface refresh errors."""

    def refresh(self, force: bool = False) -> bool:
        """Bring the index up to date. Cheap when nothing moved; applies just the
        changed records when iCloud can list them; otherwise re-reads everything.
        Returns True when a full snapshot was taken."""
        token = self.backend.change_token()
        stored = self.index.get_meta("sync_token")
        now = datetime.now(timezone.utc)
        if not force and token is not None and token == stored:
            self.index.set_meta("last_check", now.isoformat())
            return False
        if not force and token is not None and stored and self._reconciled_recently(now):
            ch = self.backend.changes(stored, Known(lambda i: self.index.asset(i) is not None,
                                                    lambda i: self.index.album(i) is not None))
            if ch is not None:
                with self._wlock:        # never overwrite the index mid-write
                    self._apply(ch)
                    self.index.set_meta("sync_token", ch.token)
                self.index.set_meta("last_check", now.isoformat())
                self.index.set_meta("last_sync", now.isoformat())
                return False
        snap = self.backend.snapshot()
        with self._wlock:
            self.index.replace_snapshot(snap)
            if token is not None:
                self.index.set_meta("sync_token", token)
            self.index.set_meta("last_full", now.isoformat())
        return True

    def _reconciled_recently(self, now: datetime) -> bool:
        """Smart albums other than the ones _apply maintains only change through a
        full read, so do one every reconcile_interval while changes keep coming."""
        last = self.index.get_meta("last_full")
        if not last:
            return False
        try:
            age = (now - datetime.fromisoformat(last)).total_seconds()
        except ValueError:
            return False
        return age < self.s.reconcile_interval

    def _apply(self, ch) -> None:
        for alb in ch.albums_gone:
            self.index.drop_album(alb)
            self.backend.forget_album(alb) if hasattr(self.backend, "forget_album") else None
        for alb in ch.albums:
            self.index.put_album(alb)
            self.backend.forget_album(alb.id) if hasattr(self.backend, "forget_album") else None
        for aid in ch.removed:
            self.index.delete_asset(aid)
        for a in ch.upserts:
            self.index.put_asset(a)
            live = not a.deleted
            self.index.set_smart("Favorites", a.id, a.favorite and live)
            self.index.set_smart("Hidden", a.id, a.hidden and live)
            self.index.set_smart("Videos", a.id, a.item_type == "movie" and live)
            self.index.set_smart("Live", a.id, a.live and live)
        for alb, aid in ch.member_add:
            for i in (aid, aid + "~raw"):
                if self.index.asset(i) is not None:
                    self.index.add_member(alb, i)
        for alb, aid in ch.member_del:
            for i in (aid, aid + "~raw"):
                self.index.remove_member(alb, i)

    # ======================= reads =======================
    def kind(self, key: str) -> tuple[str, str]:
        if key in ("root", "", "/"):
            return "root", ""
        if key in ("all", "deleted"):
            return key, ""
        if key == SHARED_DIR:
            return "shared", ""
        k, _, v = key.partition(":")
        if k in ("smart", "album", "folder") and v:
            if k == "album" and self._archive_id() == v:
                return "archive", v
            return k, v
        raise NotFound(f"unknown directory {key!r}")

    def _archive_id(self) -> str | None:
        rec = self.index.album_by_name(self.s.archive_name, None)
        return rec.id if rec else None

    def list(self, key: str) -> list[Entry]:
        kind, ref = self.kind(key)
        if kind == "root":
            return self._list_root()
        if kind == "shared":
            return self._list_shared()
        if kind == "folder":
            return self._dir_entries(self.index.child_albums(ref), set())
        if kind == "all":
            assets = self.index.all_assets()
        elif kind == "deleted":
            assets = self.index.all_assets(deleted=True)
        elif kind == "smart":
            assets = self.index.smart_assets(ref)
        else:  # album / archive
            if ref.startswith(SHARED_PREFIX):
                if not self.s.show_shared_albums:
                    raise NotFound(key)
                self._kick(f"album:{ref}", lambda: self._refresh_shared_album(ref),
                           self.s.fresh_max_age)
            if not self.index.album(ref):
                raise NotFound(key)
            assets = self.index.album_assets(ref)
        names = self.index.display_names(key, assets, naming.unique_file_name)
        return [self._file_entry(key, a, names[a.id]) for a in assets]

    def _list_root(self) -> list[Entry]:
        out = [Entry(id="all", name=self.s.root_names["all"], is_dir=True)]
        if self.s.show_smart_albums:
            for n in self.index.smart_names():
                if n in SMART_LABELS:
                    out.append(Entry(id=f"smart:{n}", name=SMART_LABELS[n], is_dir=True))
        out.append(Entry(id="deleted", name=self.s.root_names["deleted"], is_dir=True))
        if self.s.show_shared_albums:
            out.append(Entry(id=SHARED_DIR, name=self.s.root_names["shared"], is_dir=True))
        out.extend(self._dir_entries(self.index.child_albums(None), {e.name for e in out}))
        return out

    def _dir_entries(self, albums: list[AlbumRec], taken: set[str]) -> list[Entry]:
        """Directory entries with unique names: iCloud allows several albums (or
        folders) with one name, and a name clash hides all but one of them. The
        first holder (lowest id, so the choice is stable) keeps the name, later
        ones get `name~a3f9`; `taken` is pre-filled with reserved root names."""
        taken = set(taken)
        out = []
        for a in sorted(albums, key=lambda a: (naming.dir_name(a.name), a.id)):
            e = self._dir_entry(a)
            base = e.name
            if base in taken:
                for length in (4, 6, 8, 40):
                    e.name = f"{base}~{naming.short_id(a.id, length)}"
                    if e.name not in taken:
                        break
            taken.add(e.name)
            out.append(e)
        return out

    def _dir_entry(self, a: AlbumRec) -> Entry:
        return Entry(id=f"{'folder' if a.is_folder else 'album'}:{a.id}",
                     name=naming.dir_name(a.name), is_dir=True)

    # ======================= shared photo streams =======================
    # Read-only, `s:`-prefixed ids, and no change feed to ride: each scope (the
    # album list, one album) refreshes on demand with the same shape as
    # freshen() -- kick a background re-read, wait at most fresh_wait, serve
    # what we have. A shared failure never touches the private refresh.
    def _kick(self, key: str, fn: Callable[[], None], max_age: float) -> None:
        with self._shared_lock:
            ev = self._shared_inflight.get(key)
            if ev is None:
                if time.monotonic() - self._shared_tries.get(key, float("-inf")) < max_age:
                    return
                self._shared_tries[key] = time.monotonic()
                ev = threading.Event()
                self._shared_inflight[key] = ev

                def run() -> None:
                    try:
                        fn()
                    except Exception as e:
                        with self._shared_lock:
                            self._shared_tries.pop(key, None)   # retry on the next open
                        log.warning("shared refresh (%s) failed: %s", key, e)
                    finally:
                        with self._shared_lock:
                            self._shared_inflight.pop(key, None)
                        ev.set()
                threading.Thread(target=run, daemon=True).start()
        ev.wait(self.s.fresh_wait)

    def _list_shared(self) -> list[Entry]:
        if not self.s.show_shared_albums:
            raise NotFound("shared albums are hidden (BRIDGE_SHOW_SHARED_ALBUMS=0)")
        self._kick("shared:list", self._refresh_shared_list, self.s.fresh_max_age)
        return self._dir_entries(self.index.child_albums(SHARED_PARENT), set())

    def _refresh_shared_list(self) -> None:
        """Re-read the shared album list and reconcile it into the index."""
        fresh = {r.id: replace(r, parent_id=SHARED_PARENT)
                 for r in self.backend.shared_albums()}
        with self._wlock:
            cur = {a.id: a for a in self.index.child_albums(SHARED_PARENT)}
            for aid, rec in fresh.items():
                old = cur.get(aid)
                if old is None or old.name != rec.name:
                    self.index.put_album(rec)   # members/names of this id survive
            for aid in cur:
                if aid not in fresh:
                    self.index.drop_album(aid)  # drops members and names too
            self.index.prune_shared_assets()   # and assets of albums gone for good
        self.index.set_meta("shared_last_sync", datetime.now(timezone.utc).isoformat())

    def _refresh_shared_album(self, album_id: str) -> None:
        """Re-read one shared album's assets and swap them into the index."""
        alb, assets = self.backend.shared_album_assets(album_id)
        alb = replace(alb, parent_id=SHARED_PARENT)
        ids = {a.id for a in assets}
        with self._wlock:
            cur = {a.id for a in self.index.album_assets(album_id)}
            self.index.put_album(alb)           # the album appears even when the
            for a in assets:                    # list refresh has not run yet
                self.index.put_asset(a)
                self.index.add_member(album_id, a.id)
            for gone in cur - ids:              # a shared asset lives in one album
                self.index.delete_asset(gone)
        self.index.set_meta("shared_last_sync", datetime.now(timezone.utc).isoformat())

    def _file_entry(self, key: str, a: AssetRec, name: str) -> Entry:
        st = self.index.asset_status(a.id)
        status = st[0] if st else "ready"
        # The driver only checks that a thumbnail exists and fetches it through
        # /v1/raw, so no iCloud URL (and no backend call) is needed per entry.
        thumb = "" if status != "ready" else f"/v1/raw?id={file_id(key, a.id)}&version=thumb"
        return Entry(id=file_id(key, a.id), name=name, is_dir=False, size=a.size,
                     modified=a.modified.isoformat(), created=a.created.isoformat(),
                     thumb=thumb, status=status)

    def link(self, fid: str, version: str = "original", refresh: bool = False) -> dict:
        """Fresh signed URL every call; a staged local copy while uploading.

        refresh=True forces the backend to re-read the asset record, which is what
        a caller does after iCloud answers 410 to a URL it was just given.
        """
        _, asset_id = parse_file_id(fid)
        st = self.index.asset_status(asset_id)
        if st is None:
            raise NotFound(fid)
        status, staged = st
        if status != "ready":
            if staged and Path(staged).exists():
                return {"local": True, "path": staged}
            raise NotFound(f"{fid} is {status} and has no staged copy")
        if refresh:
            fresh = getattr(self.backend, "force_requery", None)
            if fresh is not None:
                fresh(asset_id)
        fresh_url = getattr(self.backend, "fresh_url", None)
        url = fresh_url(asset_id, version) if fresh_url else self.backend.download_url(asset_id, version)
        if not url:
            raise NotFound(f"no {version} resource for {fid}")
        return {"local": False, "url": url}

    def cached_thumb(self, fid: str) -> Path | None:
        """Thumbnail bytes already on disk for this file id, if any."""
        if self.s.thumb_cache_bytes <= 0:
            return None
        _, asset_id = parse_file_id(fid)
        return self.thumbs.get(asset_id)

    def store_thumb(self, fid: str, data: bytes, content_type: str | None) -> None:
        """Remember a thumbnail so the next view is a local read."""
        if self.s.thumb_cache_bytes <= 0:
            return
        _, asset_id = parse_file_id(fid)
        self.thumbs.put(asset_id, data, content_type)

    # ======================= guards =======================
    def _writable(self) -> None:
        if self.s.read_only:
            raise ReadOnly("BRIDGE_READ_ONLY is on: all writes are refused")

    def _need_asset(self, asset_id: str) -> None:
        # Shared-stream assets are read-only for good, whatever the switches
        # say: they belong to another member's album, and shared streams have
        # no write path anyway. This check does not depend on the whitelist.
        if asset_id.startswith(SHARED_PREFIX):
            raise Forbidden(f"{asset_id} is in a shared album: shared albums are read-only")
        # Live Photos are read-only for good: the still and its paired video must
        # stay together, and this check does not depend on the whitelist.
        if asset_id.endswith("~raw"):
            raise Forbidden(f"{asset_id} is the RAW half of a photo and is read-only")
        rec = self.index.asset(asset_id)
        if rec is not None and rec.live:
            raise Forbidden(f"live photo {asset_id} is read-only")
        if self.s.whitelist and not self.index.is_owned("asset", asset_id):
            raise Forbidden(f"whitelist: asset {asset_id} was not created by the bridge")

    def _need_album(self, album_id: str) -> None:
        if album_id.startswith(SHARED_PREFIX):
            raise Forbidden(f"{album_id} is a shared album: shared albums are read-only")
        if self.s.whitelist and not self.index.is_owned("album", album_id):
            raise Forbidden(f"whitelist: album {album_id} was not created by the bridge")

    def _need_name(self, name: str) -> None:
        if "/" in name or not name.strip():
            raise OpError(f"invalid name {name!r}")
        if self.s.whitelist and not name.startswith(self.s.test_prefix):
            raise Forbidden(f"whitelist: album names must start with {self.s.test_prefix}")

    def _write(self, op: str, args: dict, fn: Callable[[], object]) -> object:
        """Run one write under the global write lock with audit and guards."""
        self._writable()
        with self._wlock:
            try:
                result = fn()
            except OpError as e:
                self.audit.write(op, args, error=f"refused: {e}")
                raise
            except Exception as e:
                self.audit.write(op, args, error=f"{type(e).__name__}: {e}")
                raise
            self.audit.write(op, args, result=result)
            return result

    # ======================= archive =======================
    def ensure_archive(self) -> str:
        """Called with the write lock held."""
        aid = self._archive_id()
        if aid:
            return aid
        self._need_name(self.s.archive_name)
        rec = self.backend.create_album(self.s.archive_name, None)
        self.index.put_album(rec)
        self.index.own("album", rec.id)
        self.audit.write("create_archive", {"name": self.s.archive_name}, result=rec.id)
        return rec.id

    def _archive(self, asset_id: str) -> None:
        aid = self.ensure_archive()
        self._need_album(aid)
        self.backend.add_to_album(asset_id, aid)
        self.index.add_member(aid, asset_id)

    # ======================= directory ops =======================
    def mkdir(self, parent: str, name: str) -> Entry:
        def run():
            kind, ref = self.kind(parent)
            if kind not in ("root", "folder"):
                raise Forbidden(f"cannot create an album inside {parent}")
            self._need_name(name)
            parent_id = ref or None
            if parent_id:
                self._need_album(parent_id)
            if self.index.album_by_name(name, parent_id):
                raise Conflict(f"album {name!r} exists")
            rec = self.backend.create_album(name, parent_id)
            self.index.put_album(rec)
            self.index.own("album", rec.id)
            return self._dir_entry(rec)
        return self._write("mkdir", {"parent": parent, "name": name}, run)

    def rename(self, obj_id: str, name: str) -> Entry:
        def run():
            if obj_id.startswith("f|"):
                raise Forbidden("renaming photos is not supported")
            kind, ref = self.kind(obj_id)
            if kind not in ("album", "folder"):
                raise Forbidden(f"cannot rename {obj_id}")
            self._need_album(ref)
            self._need_name(name)
            old = self.index.album(ref)
            self.backend.rename_album(ref, name)
            new = AlbumRec(id=ref, name=name, parent_id=old.parent_id, is_folder=old.is_folder)
            self.index.put_album(new)
            return self._dir_entry(new)
        return self._write("rename", {"id": obj_id, "name": name}, run)

    def _remove_dir(self, key: str) -> dict:
        kind, ref = self.kind(key)
        if kind == "folder":
            self._need_album(ref)
            if self.index.child_albums(ref):
                raise Conflict("folder is not empty")
            self.backend.delete_album(ref)
            self.index.drop_album(ref)
            self.index.disown("album", ref)
            return {"deleted_folder": ref}
        if kind != "album":
            raise Forbidden(f"cannot delete {key}")
        self._need_album(ref)
        members = [a.id for a in self.index.album_assets(ref)]
        for m in members:
            self._need_asset(m)
        for m in members:                      # archive first, then delete album
            self._archive(m)
        self.backend.delete_album(ref)
        self.index.drop_album(ref)
        self.index.disown("album", ref)
        return {"deleted_album": ref, "archived": members}

    # ======================= file ops =======================
    def remove(self, obj_id: str) -> dict:
        def run():
            if not obj_id.startswith("f|"):
                return self._remove_dir(obj_id)
            key, asset_id = parse_file_id(obj_id)
            kind, ref = self.kind(key)
            if kind in ("archive", "deleted", "root"):
                raise Forbidden(f"deleting from {kind} is not allowed")
            self._need_asset(asset_id)
            if kind == "album":
                self._need_album(ref)
                self._archive(asset_id)
                self.backend.remove_from_album(asset_id, ref)
                self.index.remove_member(ref, asset_id)
                return {"archived": asset_id, "removed_from": ref}
            self._archive(asset_id)        # all / smart: photo itself untouched
            return {"archived": asset_id}
        return self._write("remove", {"id": obj_id}, run)

    def move(self, fid: str, to: str) -> dict:
        def run():
            if not fid.startswith("f|"):
                raise Forbidden("moving albums is not supported")
            src_key, asset_id = parse_file_id(fid)
            src_kind, src = self.kind(src_key)
            dst_kind, dst = self.kind(to)
            self._need_asset(asset_id)
            if src_kind == "deleted":                       # restore
                self.backend.set_flag(asset_id, "deleted", False)
                self.index.set_flag(asset_id, "deleted", False)
                if dst_kind in ("album", "archive"):
                    self._need_album(dst)
                    self.backend.add_to_album(asset_id, dst)
                    self.index.add_member(dst, asset_id)
                return {"recovered": asset_id, "to": to}
            if dst_kind not in ("album", "archive"):
                raise Forbidden(f"cannot move into {to}")
            if src_kind == "archive":
                raise Forbidden("move out of the archive is a copy; use copy")
            self._need_album(dst)
            self.backend.add_to_album(asset_id, dst)       # add B first ...
            self.index.add_member(dst, asset_id)
            if src_kind == "album" and src != dst:          # ... then leave A
                self._need_album(src)
                self.backend.remove_from_album(asset_id, src)
                self.index.remove_member(src, asset_id)
            return {"moved": asset_id, "from": src_key, "to": to}
        return self._write("move", {"id": fid, "to": to}, run)

    def copy(self, fid: str, to: str) -> dict:
        def run():
            _, asset_id = parse_file_id(fid)
            dst_kind, dst = self.kind(to)
            if dst_kind != "album":
                raise Forbidden(f"cannot copy into {to}")
            self._need_asset(asset_id)
            self._need_album(dst)
            self.backend.add_to_album(asset_id, dst)
            self.index.add_member(dst, asset_id)
            return {"copied": asset_id, "to": to}
        return self._write("copy", {"id": fid, "to": to}, run)

    def other(self, op: str, fid: str) -> dict:
        if op not in OTHER_OPS:
            raise OpError(f"unknown op {op}")

        def run():
            _, asset_id = parse_file_id(fid)
            self._need_asset(asset_id)
            flag, value = {
                "favorite": ("favorite", True), "unfavorite": ("favorite", False),
                "hide": ("hidden", True), "unhide": ("hidden", False),
                "recover": ("deleted", False), "purge": ("deleted", True),
            }[op]
            self.backend.set_flag(asset_id, flag, value)
            self.index.set_flag(asset_id, flag, value)
            return {op: asset_id}
        return self._write(f"other:{op}", {"id": fid}, run)

    # ======================= upload =======================
    def upload(self, parent: str, name: str, body: BinaryIO) -> Entry:
        """Store the bytes, show a placeholder at once, push to iCloud in the
        background. A same-name file in `parent` triggers the overwrite flow."""
        self._writable()
        kind, ref = self.kind(parent)
        if kind not in ("all", "album"):
            raise Forbidden(f"cannot upload into {parent}")
        if kind == "album":
            self._need_album(ref)
        if "/" in name:
            raise OpError("invalid file name")
        existing = {e.name: e for e in self.list(parent)}
        old_id = None
        if name in existing:
            old_id = parse_file_id(existing[name].id)[1]
            self._need_asset(old_id)

        pid = f"pending-{uuid.uuid4().hex}"
        staged = self.s.staging_dir / pid
        with staged.open("wb") as f:
            shutil.copyfileobj(body, f, length=1 << 20)
        now = datetime.now(timezone.utc)
        ph = AssetRec(id=pid, filename=name, size=staged.stat().st_size,
                      created=now, modified=now)
        with self._wlock:
            self.index.put_asset(ph, status="processing", staged_path=str(staged))
            self.index.own("asset", pid)
            if kind == "album":
                self.index.add_member(ref, pid)
            self.index.display_names(parent, [ph], lambda a, taken: name
                                     if name not in taken else naming.unique_file_name(a, taken))
            if old_id:
                with self.index._lock, self.index.db:  # placeholder takes the name now
                    self.index.db.execute("DELETE FROM names WHERE dir_key=? AND asset_id=?",
                                          (parent, pid))
                    self.index.db.execute(
                        "UPDATE names SET asset_id=? WHERE dir_key=? AND asset_id=?",
                        (pid, parent, old_id))
        self.audit.write("upload:accepted", {"parent": parent, "name": name,
                                             "overwrite": old_id}, result=pid)
        fut = self._uploads.submit(self._finish_upload, pid, parent, name, old_id)
        self._pending.append(fut)
        return self._file_entry(parent, ph, name)

    def _finish_upload(self, pid: str, parent: str, name: str, old_id: str | None) -> None:
        kind, ref = self.kind(parent)
        staged = self.s.staging_dir / pid
        args = {"placeholder": pid, "parent": parent, "name": name, "overwrite": old_id}
        step = "a:read-old"
        try:
            with self._wlock:
                before = None
                if old_id:   # (a) remember albums and flags of the photo being replaced
                    old = self.index.asset(old_id)
                    before = {"albums": [x for x in self.index.albums_of(old_id)
                                         if x != self._archive_id()],
                              "favorite": old.favorite, "hidden": old.hidden}
                    for album_id in before["albums"]:   # refuse before touching iCloud
                        self._need_album(album_id)
                    self.audit.write("overwrite", args, step=step, before=before)
                step = "b:upload"   # (b) upload and wait until iCloud has processed it
                rec = self.backend.upload(staged, name)
                self.index.replace_asset_id(pid, rec)
                self.index.own("asset", rec.id)
                self.audit.write("upload", args, step=step, result=rec.id)
                step = "c:restore-albums"   # (c) put the new photo where the old one was
                targets = set(before["albums"]) if before else set()
                if kind == "album":
                    targets.add(ref)
                for album_id in targets:
                    self._need_album(album_id)
                    self.backend.add_to_album(rec.id, album_id)
                    self.index.add_member(album_id, rec.id)
                if before:
                    for flag in ("favorite", "hidden"):
                        if before[flag]:
                            self.backend.set_flag(rec.id, flag, True)
                            self.index.set_flag(rec.id, flag, True)
                    step = "d:archive-old"   # (d) archive the old one, take it out
                    self._archive(old_id)
                    for album_id in before["albums"]:
                        self.backend.remove_from_album(old_id, album_id)
                        self.index.remove_member(album_id, old_id)
                    self.audit.write("overwrite", args, step="done",
                                     after={"new": rec.id, "albums": sorted(targets)})
            staged.unlink(missing_ok=True)
        except Exception as e:  # stop here and keep the staged file for inspection
            self.index.set_status(pid, "failed")
            self.audit.write("upload", args, step=step, error=f"{type(e).__name__}: {e}")
            raise

    def wait_uploads(self, timeout: float = 60) -> None:
        """Block until queued uploads finish (tests, shutdown)."""
        deadline = time.monotonic() + timeout
        for f in list(self._pending):
            try:
                f.result(timeout=max(0.0, deadline - time.monotonic()))
            except Exception:
                pass
        self._pending = [f for f in self._pending if not f.done()]

    # ======================= cleanup (6.6) =======================
    def cleanup_plan(self) -> dict:
        return {
            "albums": [{"id": a, "name": getattr(self.index.album(a), "name", None)}
                       for a in self.index.owned("album")],
            "assets": [{"id": a, "name": getattr(self.index.asset(a), "filename", None)}
                       for a in self.index.owned("asset") if not a.startswith("pending-")],
        }

    def cleanup(self, confirm: bool) -> dict:
        plan = self.cleanup_plan()
        if not confirm:
            return {"preview": plan}

        def run():
            done = {"albums": [], "assets": []}
            for a in plan["albums"]:
                if self.index.album(a["id"]):
                    self.backend.delete_album(a["id"])
                    self.index.drop_album(a["id"])
                self.index.disown("album", a["id"])
                done["albums"].append(a["id"])
            for a in plan["assets"]:          # real delete: goes to 最近删除, 30 days
                if self.index.asset(a["id"]):
                    self.backend.set_flag(a["id"], "deleted", True)
                    self.index.set_flag(a["id"], "deleted", True)
                done["assets"].append(a["id"])
            return done
        return self._write("cleanup", {"count": {k: len(v) for k, v in plan.items()}}, run)

    def close(self) -> None:
        self._uploads.shutdown(wait=True)


__all__ = ["Ops", "OpError", "Forbidden", "NotFound", "Conflict", "ReadOnly",
           "BackendError", "file_id", "parse_file_id"]
