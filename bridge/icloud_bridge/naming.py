"""File and directory names shown to OpenList."""

from __future__ import annotations

import hashlib

from .backend import AssetRec

# Characters that break paths. rclone #9911: a suffix must never contain "/".
_DIR_TRANSLATE = str.maketrans({"/": "／", "\\": "＼", "\0": ""})


def short_id(asset_id: str, length: int = 4) -> str:
    """Hex digest, so it can only contain [0-9a-f]."""
    return hashlib.sha1(asset_id.encode()).hexdigest()[:length]


def safe_name(name: str) -> str:
    name = name.translate(_DIR_TRANSLATE).strip()
    return name or "未命名"


def dir_name(album_name: str) -> str:
    return safe_name(album_name)


def unique_file_name(asset: AssetRec, taken: set[str]) -> str:
    """First holder of a name keeps it; later ones get IMG_0001~a3f9.HEIC."""
    base = safe_name(asset.filename)
    if base not in taken:
        return base
    stem, dot, ext = base.rpartition(".")
    if not dot:
        stem, ext = base, ""
    for length in (4, 6, 8, 40):
        candidate = f"{stem}~{short_id(asset.id, length)}{dot}{ext}"
        if candidate not in taken:
            return candidate
    raise ValueError(f"cannot name {asset.id}")  # sha1 collision; never expected
