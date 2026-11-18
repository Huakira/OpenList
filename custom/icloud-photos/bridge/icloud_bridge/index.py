"""Local SQLite index: the bridge answers every read from here."""

from __future__ import annotations

import sqlite3
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterable

from .backend import AlbumRec, AssetRec, Snapshot

SCHEMA = """
CREATE TABLE IF NOT EXISTS assets (
  id TEXT PRIMARY KEY, filename TEXT NOT NULL, size INTEGER NOT NULL,
  created TEXT NOT NULL, modified TEXT NOT NULL, item_type TEXT NOT NULL,
  favorite INTEGER NOT NULL DEFAULT 0, hidden INTEGER NOT NULL DEFAULT 0,
  deleted INTEGER NOT NULL DEFAULT 0, live INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'ready',     -- ready | processing | failed
  staged_path TEXT
);
CREATE TABLE IF NOT EXISTS albums (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, parent_id TEXT,
  is_folder INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS members (
  album_id TEXT NOT NULL, asset_id TEXT NOT NULL, PRIMARY KEY (album_id, asset_id)
);
CREATE TABLE IF NOT EXISTS smart (
  name TEXT NOT NULL, asset_id TEXT NOT NULL, PRIMARY KEY (name, asset_id)
);
-- display names are assigned once per directory and then kept stable
CREATE TABLE IF NOT EXISTS names (
  dir_key TEXT NOT NULL, asset_id TEXT NOT NULL, display TEXT NOT NULL,
  PRIMARY KEY (dir_key, asset_id), UNIQUE (dir_key, display)
);
-- write whitelist: what this service itself created (plan section 6.2)
CREATE TABLE IF NOT EXISTS owned (
  kind TEXT NOT NULL, id TEXT NOT NULL, created_at TEXT NOT NULL,
  PRIMARY KEY (kind, id)
);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
"""


def _ts(d: datetime) -> str:
    return d.astimezone(timezone.utc).isoformat()


def _row_asset(r: sqlite3.Row) -> AssetRec:
    return AssetRec(
        id=r["id"], filename=r["filename"], size=r["size"],
        created=datetime.fromisoformat(r["created"]),
        modified=datetime.fromisoformat(r["modified"]),
        item_type=r["item_type"], favorite=bool(r["favorite"]),
        hidden=bool(r["hidden"]), deleted=bool(r["deleted"]), live=bool(r["live"]),
    )


def _row_album(r: sqlite3.Row) -> AlbumRec:
    return AlbumRec(id=r["id"], name=r["name"], parent_id=r["parent_id"],
                    is_folder=bool(r["is_folder"]))


class Index:
    def __init__(self, path: Path | str) -> None:
        self._lock = threading.RLock()
        self.db = sqlite3.connect(str(path), check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.executescript(SCHEMA)
        cols = {r["name"] for r in self.db.execute("PRAGMA table_info(assets)")}
        if "live" not in cols:      # index created before the live-photo guard
            self.db.execute("ALTER TABLE assets ADD COLUMN live INTEGER NOT NULL DEFAULT 0")

    # ---------- bulk sync ----------
    def replace_snapshot(self, snap: Snapshot) -> None:
        """Swap in a private-library snapshot. Shared-stream rows (`s:` ids,
        parent 'shared') are refreshed on demand and never wiped here."""
        with self._lock, self.db:
            self.db.execute("DELETE FROM assets WHERE status = 'ready' AND id NOT LIKE 's:%'")
            self.db.execute("DELETE FROM albums WHERE parent_id IS NULL OR parent_id <> 'shared'")
            self.db.execute("DELETE FROM members WHERE album_id NOT LIKE 's:%'")
            self.db.execute("DELETE FROM smart")
            self.db.executemany(
                "INSERT OR REPLACE INTO assets (id, filename, size, created, modified,"
                " item_type, favorite, hidden, deleted, live, status) VALUES (?,?,?,?,?,?,?,?,?,?,'ready')",
                [(a.id, a.filename, a.size, _ts(a.created), _ts(a.modified), a.item_type,
                  int(a.favorite), int(a.hidden), int(a.deleted), int(a.live)) for a in snap.assets],
            )
            self.db.executemany(
                "INSERT INTO albums VALUES (?,?,?,?)",
                [(l.id, l.name, l.parent_id, int(l.is_folder)) for l in snap.albums],
            )
            self.db.executemany(
                "INSERT OR IGNORE INTO members VALUES (?,?)",
                [(aid, sid) for aid, ids in snap.memberships.items() for sid in ids],
            )
            self.db.executemany(
                "INSERT OR IGNORE INTO smart VALUES (?,?)",
                [(n, sid) for n, ids in snap.smart.items() for sid in ids],
            )
            self.set_meta("last_sync", _ts(datetime.now(timezone.utc)))

    # ---------- reads ----------
    def asset(self, asset_id: str) -> AssetRec | None:
        r = self.db.execute("SELECT * FROM assets WHERE id=?", (asset_id,)).fetchone()
        return _row_asset(r) if r else None

    def asset_status(self, asset_id: str) -> tuple[str, str | None] | None:
        r = self.db.execute("SELECT status, staged_path FROM assets WHERE id=?",
                            (asset_id,)).fetchone()
        return (r["status"], r["staged_path"]) if r else None

    def album(self, album_id: str) -> AlbumRec | None:
        r = self.db.execute("SELECT * FROM albums WHERE id=?", (album_id,)).fetchone()
        return _row_album(r) if r else None

    def album_by_name(self, name: str, parent_id: str | None = None) -> AlbumRec | None:
        r = self.db.execute(
            "SELECT * FROM albums WHERE name=? AND parent_id IS ?", (name, parent_id)
        ).fetchone()
        return _row_album(r) if r else None

    def child_albums(self, parent_id: str | None) -> list[AlbumRec]:
        rows = self.db.execute(
            "SELECT * FROM albums WHERE parent_id IS ? ORDER BY name", (parent_id,)
        ).fetchall()
        return [_row_album(r) for r in rows]

    def album_assets(self, album_id: str) -> list[AssetRec]:
        rows = self.db.execute(
            "SELECT a.* FROM assets a JOIN members m ON m.asset_id=a.id"
            " WHERE m.album_id=? AND a.deleted=0 ORDER BY a.created", (album_id,)
        ).fetchall()
        return [_row_asset(r) for r in rows]

    def smart_assets(self, name: str) -> list[AssetRec]:
        rows = self.db.execute(
            "SELECT a.* FROM assets a JOIN smart s ON s.asset_id=a.id"
            " WHERE s.name=? AND a.deleted=0 ORDER BY a.created", (name,)
        ).fetchall()
        return [_row_asset(r) for r in rows]

    def smart_names(self) -> list[str]:
        return [r[0] for r in self.db.execute("SELECT DISTINCT name FROM smart ORDER BY name")]

    def all_assets(self, deleted: bool = False) -> list[AssetRec]:
        rows = self.db.execute(
            "SELECT * FROM assets WHERE deleted=? AND hidden=0 AND id NOT LIKE 's:%'"
            " ORDER BY created", (int(deleted),),
        ).fetchall() if not deleted else self.db.execute(
            "SELECT * FROM assets WHERE deleted=1 AND id NOT LIKE 's:%' ORDER BY created").fetchall()
        return [_row_asset(r) for r in rows]

    def albums_of(self, asset_id: str) -> list[str]:
        return [r[0] for r in self.db.execute(
            "SELECT album_id FROM members WHERE asset_id=?", (asset_id,))]

    # ---------- display names ----------
    def display_names(self, dir_key: str, assets: Iterable[AssetRec],
                      namer) -> dict[str, str]:
        """Return asset_id -> display name, assigning and persisting new ones."""
        assets = list(assets)
        with self._lock, self.db:
            existing = {r["asset_id"]: r["display"] for r in self.db.execute(
                "SELECT asset_id, display FROM names WHERE dir_key=?", (dir_key,))}
            taken = set(existing.values())
            out: dict[str, str] = {}
            for a in assets:
                name = existing.get(a.id)
                if name is None:
                    name = namer(a, taken)
                    taken.add(name)
                    self.db.execute("INSERT INTO names VALUES (?,?,?)", (dir_key, a.id, name))
                out[a.id] = name
            return out

    def forget_name(self, dir_key: str, asset_id: str) -> None:
        with self._lock, self.db:
            self.db.execute("DELETE FROM names WHERE dir_key=? AND asset_id=?",
                            (dir_key, asset_id))

    # ---------- local mutations mirrored after a successful write ----------
    def put_asset(self, a: AssetRec, status: str = "ready",
                  staged_path: str | None = None) -> None:
        with self._lock, self.db:
            self.db.execute(
                "INSERT OR REPLACE INTO assets (id, filename, size, created, modified,"
                " item_type, favorite, hidden, deleted, live, status, staged_path)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                (a.id, a.filename, a.size, _ts(a.created), _ts(a.modified), a.item_type,
                 int(a.favorite), int(a.hidden), int(a.deleted), int(a.live), status, staged_path),
            )

    def replace_asset_id(self, old: str, new: AssetRec) -> None:
        """Swap an upload placeholder for the real iCloud record."""
        with self._lock, self.db:
            self.db.execute("DELETE FROM assets WHERE id=?", (old,))
            self.put_asset(new)
            for table, col in (("members", "asset_id"), ("names", "asset_id")):
                self.db.execute(f"UPDATE {table} SET {col}=? WHERE {col}=?", (new.id, old))
            self.db.execute("UPDATE owned SET id=? WHERE kind='asset' AND id=?", (new.id, old))

    def set_status(self, asset_id: str, status: str) -> None:
        with self._lock, self.db:
            self.db.execute("UPDATE assets SET status=? WHERE id=?", (status, asset_id))

    def set_flag(self, asset_id: str, flag: str, value: bool) -> None:
        assert flag in {"favorite", "hidden", "deleted"}
        with self._lock, self.db:
            self.db.execute(f"UPDATE assets SET {flag}=? WHERE id=?", (int(value), asset_id))

    def delete_asset(self, asset_id: str) -> None:
        """Forget an asset that is gone from iCloud (not the archive move)."""
        with self._lock, self.db:
            for table, col in (("assets", "id"), ("members", "asset_id"),
                               ("smart", "asset_id"), ("names", "asset_id")):
                self.db.execute(f"DELETE FROM {table} WHERE {col}=?", (asset_id,))

    def set_smart(self, name: str, asset_id: str, present: bool) -> None:
        with self._lock, self.db:
            if present:
                self.db.execute("INSERT OR IGNORE INTO smart VALUES (?,?)", (name, asset_id))
            else:
                self.db.execute("DELETE FROM smart WHERE name=? AND asset_id=?",
                                (name, asset_id))

    def put_album(self, a: AlbumRec) -> None:
        with self._lock, self.db:
            self.db.execute("INSERT OR REPLACE INTO albums VALUES (?,?,?,?)",
                            (a.id, a.name, a.parent_id, int(a.is_folder)))

    def prune_shared_assets(self) -> None:
        """Drop shared assets no album points at any more (a shared asset
        lives in exactly one album), plus their display-name rows."""
        with self._lock, self.db:
            self.db.execute(
                "DELETE FROM assets WHERE id LIKE 's:%'"
                " AND id NOT IN (SELECT asset_id FROM members)")
            self.db.execute(
                "DELETE FROM names WHERE asset_id LIKE 's:%'"
                " AND asset_id NOT IN (SELECT asset_id FROM members)")

    def drop_album(self, album_id: str) -> None:
        with self._lock, self.db:
            self.db.execute("DELETE FROM albums WHERE id=?", (album_id,))
            self.db.execute("DELETE FROM members WHERE album_id=?", (album_id,))
            self.db.execute("DELETE FROM names WHERE dir_key=?", (f"album:{album_id}",))

    def add_member(self, album_id: str, asset_id: str) -> None:
        with self._lock, self.db:
            self.db.execute("INSERT OR IGNORE INTO members VALUES (?,?)", (album_id, asset_id))

    def remove_member(self, album_id: str, asset_id: str) -> None:
        with self._lock, self.db:
            self.db.execute("DELETE FROM members WHERE album_id=? AND asset_id=?",
                            (album_id, asset_id))
            self.db.execute("DELETE FROM names WHERE dir_key=? AND asset_id=?",
                            (f"album:{album_id}", asset_id))

    # ---------- whitelist ----------
    def own(self, kind: str, obj_id: str) -> None:
        with self._lock, self.db:
            self.db.execute("INSERT OR IGNORE INTO owned VALUES (?,?,?)",
                            (kind, obj_id, _ts(datetime.now(timezone.utc))))

    def disown(self, kind: str, obj_id: str) -> None:
        with self._lock, self.db:
            self.db.execute("DELETE FROM owned WHERE kind=? AND id=?", (kind, obj_id))

    def is_owned(self, kind: str, obj_id: str) -> bool:
        return self.db.execute("SELECT 1 FROM owned WHERE kind=? AND id=?",
                               (kind, obj_id)).fetchone() is not None

    def owned(self, kind: str) -> list[str]:
        return [r[0] for r in self.db.execute(
            "SELECT id FROM owned WHERE kind=? ORDER BY created_at", (kind,))]

    # ---------- meta ----------
    def set_meta(self, key: str, value: str) -> None:
        with self._lock, self.db:
            self.db.execute("INSERT OR REPLACE INTO meta VALUES (?,?)", (key, value))

    def get_meta(self, key: str) -> str | None:
        r = self.db.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return r[0] if r else None
