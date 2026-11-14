"""Full library backup before any test write (plan 6.1). Read-only on iCloud.

    docker compose run --rm --entrypoint python icloud-bridge /app/scripts/backup.py /backup
    ... backup.py /backup --preflight   # count + size only, no download

1. Preflight: count every photo/video and sum original sizes. Refuse to start
   if the destination lacks that much free space (+5%).
2. Download with the sync built into pyicloud (originals, live-photo videos
   too, auto_delete off, so nothing local is ever removed).
3. Verify: files on disk vs items listed, written to backup-report.json.
"""

from __future__ import annotations

import json
import os
import shutil
import sys
from dataclasses import asdict
from datetime import datetime, timezone
from pathlib import Path

from pyicloud import PyiCloudService
from pyicloud.services.photos.sync import PhotoSyncOptions


def main(dest: Path, preflight_only: bool) -> int:
    data = Path(os.environ.get("BRIDGE_DATA_DIR", "/data"))
    api = PyiCloudService(os.environ["ICLOUD_APPLE_ID"],
                          cookie_directory=str(data / "session"))
    if api.requires_2fa:
        print("session needs 2FA: run `icloud-bridge login` first", file=sys.stderr)
        return 2
    photos = api.photos
    count, total = 0, 0
    for p in photos.all.photos:
        count += 1
        total += p.size or 0
    dest.mkdir(parents=True, exist_ok=True)
    free = shutil.disk_usage(dest).free
    report = {"started": datetime.now(timezone.utc).isoformat(), "dest": str(dest),
              "library_items": count, "library_bytes": total, "dest_free_bytes": free}
    print(json.dumps(report))
    if total * 1.05 > free:
        report["error"] = "not enough free space at destination"
        (dest / "backup-report.json").write_text(json.dumps(report, indent=2))
        print(report["error"], file=sys.stderr)
        return 3
    if preflight_only:
        return 0

    result = photos.sync(PhotoSyncOptions(directory=dest / "library",
                                          state_dir=dest / "state",
                                          live_photo_size="original",
                                          auto_delete=False))
    on_disk = sum(1 for f in (dest / "library").rglob("*") if f.is_file())
    report.update(finished=datetime.now(timezone.utc).isoformat(),
                  sync=asdict(result), files_on_disk=on_disk,
                  # live photos add a .mov per still, so on_disk >= items is expected
                  verified=on_disk >= count and result.listed_count >= count)
    (dest / "backup-report.json").write_text(json.dumps(report, indent=2, default=str))
    print(json.dumps({k: report[k] for k in ("library_items", "files_on_disk", "verified")}))
    return 0 if report["verified"] else 1


if __name__ == "__main__":
    sys.exit(main(Path(sys.argv[1]), "--preflight" in sys.argv[2:]))
