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
    show_shared_albums: bool = True  # shared photo streams (共享相册), read-only
    china: bool = False             # Apple ID region: China mainland (icloud.com.cn)
    backend: str = "pyicloud"       # "pyicloud" | "fake" (tests, local dev)
    sync_interval: int = 300        # safety-net background refresh; directory views refresh on demand
    fresh_max_age: float = 5.0      # a directory view re-checks iCloud when the index is older than this
    fresh_wait: float = 4.0         # ...and waits at most this long for it before serving what it has
    reconcile_interval: int = 1800  # seconds between full re-reads while changes keep arriving
    thumb_cache_bytes: int = 512 << 20   # on-disk thumbnail cache cap (0 = off)
    root_names: dict[str, str] = field(
        default_factory=lambda: {
            "all": "全部照片",
            "deleted": "最近删除",
            "shared": "共享相册",
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
        show_shared_albums=_bool("BRIDGE_SHOW_SHARED_ALBUMS", True),
        china=_bool("ICLOUD_CHINA", False),
        backend=os.environ.get("BRIDGE_BACKEND", "pyicloud"),
        sync_interval=int(os.environ.get("BRIDGE_SYNC_INTERVAL", "30")),
        fresh_max_age=float(os.environ.get("BRIDGE_FRESH_MAX_AGE", "5")),
        fresh_wait=float(os.environ.get("BRIDGE_FRESH_WAIT", "4")),
        reconcile_interval=int(os.environ.get("BRIDGE_RECONCILE_INTERVAL", "1800")),
        thumb_cache_bytes=int(os.environ.get("BRIDGE_THUMB_CACHE_BYTES", str(512 << 20))),
    )
