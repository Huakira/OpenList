"""M0: answer the five open questions on the main account (plan section 7).

Touches only what it creates: one uploaded test photo (fixed 2001 date,
__bridge_test_ marker) and one album named __bridge_test_m0_<time>.
Nothing else in the library is read for writing.

    BRIDGE_DATA_DIR=/data ICLOUD_APPLE_ID=... python scripts/m0_probe.py TEST.jpg
    python scripts/m0_probe.py --expiry URL_FILE   # long-running check for 2

Results go to $BRIDGE_DATA_DIR/m0-results.json.
"""

from __future__ import annotations

import json
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import requests

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "bridge"))
from icloud_bridge.pyicloud_backend import PyiCloudBackend  # noqa: E402

DATA = Path(os.environ.get("BRIDGE_DATA_DIR", "/data"))
OUT = DATA / "m0-results.json"


def now() -> str:
    return datetime.now(timezone.utc).isoformat()


def save(results: dict) -> None:
    OUT.write_text(json.dumps(results, ensure_ascii=False, indent=2, default=str))


def fetch_plain(url: str) -> int:
    """No cookies, no session: what a WebDAV client gets after a 302."""
    r = requests.get(url, headers={"Range": "bytes=0-15"}, timeout=30)
    return r.status_code


def members_of(be: PyiCloudBackend, album_id: str) -> list[str]:
    album = be.library.albums.get(album_id) or be._album(album_id)
    return [p.id for p in album.photos]


def main(test_file: Path) -> int:
    be = PyiCloudBackend(os.environ["ICLOUD_APPLE_ID"], DATA / "session")
    res: dict = {"started": now(), "file": test_file.name}

    # 3: upload -> readable
    t0 = time.monotonic()
    asset = be.upload(test_file, test_file.name)
    res["upload_returned_s"] = round(time.monotonic() - t0, 1)
    seen = None
    for _ in range(60):
        snap_ids = {p.id for p in be.library.all.photos}
        if asset.id in snap_ids:
            seen = round(time.monotonic() - t0, 1)
            break
        time.sleep(2)
    res["3_visible_after_s"] = seen
    save(res)

    # 1: signed URL without cookies
    url = be.download_url(asset.id, "original")
    res["1_url_host"] = url.split("/")[2] if url else None
    res["1_plain_get_status"] = fetch_plain(url) if url else None
    (DATA / "m0-url.txt").write_text(url or "")
    save(res)

    # 4: relation remove (both modes, on our own album only)
    album = be.create_album(f"__bridge_test_m0_{int(time.time())}", None)
    res["album"] = album.id
    for mode in ("isDeleted", "forceDelete"):
        be.add_to_album(asset.id, album.id)
        time.sleep(3)
        before = asset.id in members_of(be, album.id)
        import icloud_bridge.pyicloud_backend as pb
        pb.RELATION_REMOVE_MODE = mode
        err = None
        try:
            be.remove_from_album(asset.id, album.id)
        except Exception as e:
            err = f"{type(e).__name__}: {e}"
        time.sleep(3)
        after = asset.id in members_of(be, album.id)
        res[f"4_{mode}"] = {"member_before": before, "member_after": after, "error": err}
        save(res)

    # 5: hide / unhide / delete / recover on the test photo
    for flag, value in (("hidden", True), ("hidden", False),
                        ("deleted", True), ("deleted", False)):
        err = None
        try:
            be.set_flag(asset.id, flag, value)
        except Exception as e:
            err = f"{type(e).__name__}: {e}"
        time.sleep(3)
        snap = be.snapshot()
        rec = next((a for a in snap.assets if a.id == asset.id), None)
        res[f"5_{flag}={value}"] = {"error": err,
                                    "observed": getattr(rec, flag, None) if rec else "absent"}
        save(res)

    res["finished"] = now()
    res["leftovers"] = {"asset": asset.id, "album": album.id,
                        "note": "remove via the bridge cleanup after review"}
    save(res)
    print(json.dumps(res, ensure_ascii=False, indent=2, default=str))
    return 0


def expiry(url_file: Path, every: int = 600, limit_h: float = 48) -> int:
    """2: poll the same signed URL until it stops working."""
    url = url_file.read_text().strip()
    start = time.monotonic()
    while time.monotonic() - start < limit_h * 3600:
        code = fetch_plain(url)
        print(now(), code, flush=True)
        if code >= 400:
            res = json.loads(OUT.read_text()) if OUT.exists() else {}
            res["2_url_dead_after_h"] = round((time.monotonic() - start) / 3600, 2)
            save(res)
            return 0
        time.sleep(every)
    print("still valid after", limit_h, "h")
    return 0


if __name__ == "__main__":
    if sys.argv[1:2] == ["--expiry"]:
        sys.exit(expiry(Path(sys.argv[2])))
    sys.exit(main(Path(sys.argv[1])))
