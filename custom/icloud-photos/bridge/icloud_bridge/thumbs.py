"""On-disk cache for thumbnails.

A thumbnail is small (tens of kB) and never changes for a given asset, but
iCloud signs its URL for ~15 minutes and every gallery scroll re-requests it.
Caching the bytes once means the second view is a local file read: no iCloud
round trip, no signed URL, no 410.

Layout: <data_dir>/thumbs/<xx>/<asset id>.<ext>, where <xx> is the first two
characters of the id (a few thousand files per directory).
"""

from __future__ import annotations

import os
import threading
from pathlib import Path

EXT_BY_TYPE = {
    "image/jpeg": "jpg",
    "image/jpg": "jpg",
    "image/png": "png",
    "image/heic": "heic",
    "image/heif": "heif",
    "image/gif": "gif",
    "image/webp": "webp",
}


class ThumbCache:
    def __init__(self, root: Path, max_bytes: int = 512 * 1024 * 1024) -> None:
        self.root = Path(root)
        self.max_bytes = max_bytes
        self._lock = threading.Lock()
        self.root.mkdir(parents=True, exist_ok=True)

    def _dir(self, asset_id: str) -> Path:
        safe = "".join(c for c in asset_id if c.isalnum() or c in "-_")[:64] or "x"
        return self.root / safe[:2]

    def get(self, asset_id: str) -> Path | None:
        d = self._dir(asset_id)
        safe = "".join(c for c in asset_id if c.isalnum() or c in "-_")[:64] or "x"
        for ext in set(EXT_BY_TYPE.values()) | {"bin"}:
            p = d / f"{safe}.{ext}"
            if p.is_file() and p.stat().st_size > 0:
                return p
        return None

    def put(self, asset_id: str, data: bytes, content_type: str | None) -> Path | None:
        if not data:
            return None
        ext = EXT_BY_TYPE.get((content_type or "").split(";")[0].strip().lower(), "bin")
        d = self._dir(asset_id)
        safe = "".join(c for c in asset_id if c.isalnum() or c in "-_")[:64] or "x"
        d.mkdir(parents=True, exist_ok=True)
        tmp = d / f".{safe}.{os.getpid()}.{threading.get_ident()}.tmp"
        try:
            tmp.write_bytes(data)
            final = d / f"{safe}.{ext}"
            os.replace(tmp, final)     # atomic: a reader never sees a partial file
        except OSError:
            tmp.unlink(missing_ok=True)
            return None
        self._prune()
        return final

    def _prune(self) -> None:
        """Keep the directory under max_bytes, oldest first. Cheap enough to run
        on write: the tree is small and the stat calls are local."""
        if self.max_bytes <= 0:
            return
        with self._lock:
            files = []
            total = 0
            for p in self.root.rglob("*"):
                try:
                    if p.is_file():
                        st = p.stat()
                        files.append((st.st_mtime, st.st_size, p))
                        total += st.st_size
                except OSError:
                    continue
            if total <= self.max_bytes:
                return
            files.sort()
            for _mtime, size, p in files:
                if total <= self.max_bytes:
                    break
                try:
                    p.unlink()
                    total -= size
                except OSError:
                    continue

    def stats(self) -> dict[str, int]:
        count = 0
        total = 0
        for p in self.root.rglob("*"):
            try:
                if p.is_file():
                    count += 1
                    total += p.stat().st_size
            except OSError:
                continue
        return {"files": count, "bytes": total}
