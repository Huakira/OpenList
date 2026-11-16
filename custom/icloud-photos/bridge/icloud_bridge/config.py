"""Runtime settings, all from environment variables (set in deploy/.env)."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path


def _bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


@dataclass(frozen=True)
class Settings:
    token: str
    data_dir: Path
    apple_id: str
    # Safety switches (plan section 6)
    whitelist: bool = True          # only touch assets/albums this service created
    read_only: bool = False         # kill switch: every write is refused
    test_prefix: str = "__bridge_test_"
    archive_name: str = "__bridge_test_归档"
    show_smart_albums: bool = True
    china: bool = False             # Apple ID region: China mainland (icloud.com.cn)
    backend: str = "pyicloud"       # "pyicloud" | "fake" (tests, local dev)
    sync_interval: int = 300        # seconds between background refreshes
    thumb_cache_bytes: int = 512 << 20   # on-disk thumbnail cache cap (0 = off)
    root_names: dict[str, str] = field(
        default_factory=lambda: {
            "all": "全部照片",
            "deleted": "最近删除",
        }
    )

    @property
    def cookie_dir(self) -> Path:
        return self.data_dir / "session"

    @property
    def db_path(self) -> Path:
        return self.data_dir / "index.sqlite3"

    @property
    def audit_path(self) -> Path:
        return self.data_dir / "audit.jsonl"

    @property
    def staging_dir(self) -> Path:
        return self.data_dir / "staging"

    @property
    def thumbs_dir(self) -> Path:
        return self.data_dir / "thumbs"


def load() -> Settings:
    token = os.environ.get("BRIDGE_TOKEN", "")
    if not token:
        raise RuntimeError("BRIDGE_TOKEN is required")
    return Settings(
        token=token,
        data_dir=Path(os.environ.get("BRIDGE_DATA_DIR", "/data")),
        apple_id=os.environ.get("ICLOUD_APPLE_ID", ""),
        whitelist=_bool("BRIDGE_WRITE_WHITELIST", True),
        read_only=_bool("BRIDGE_READ_ONLY", False),
        test_prefix=os.environ.get("BRIDGE_TEST_PREFIX", "__bridge_test_"),
        archive_name=os.environ.get("BRIDGE_ARCHIVE_NAME", "__bridge_test_归档"),
        show_smart_albums=_bool("BRIDGE_SHOW_SMART_ALBUMS", True),
        china=_bool("ICLOUD_CHINA", False),
        backend=os.environ.get("BRIDGE_BACKEND", "pyicloud"),
        sync_interval=int(os.environ.get("BRIDGE_SYNC_INTERVAL", "300")),
        thumb_cache_bytes=int(os.environ.get("BRIDGE_THUMB_CACHE_BYTES", str(512 << 20))),
    )
