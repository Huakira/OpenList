"""Generate test media for the main-account tests (plan 6.3).

Every file carries a unique marker (EXIF ImageDescription / QuickTime comment)
and a fixed capture date far in the past, so it never lands in the recent
timeline and can be found again by marker.

    python scripts/gen_test_media.py OUT_DIR

Needs: Pillow, pillow-heif (HEIC), ffmpeg (MOV), exiftool (live photo pair).
Formats that cannot be produced are reported and skipped, never faked.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
import uuid
from pathlib import Path

FIXED_DATE = "2001:01:01 12:00:00"
PREFIX = "__bridge_test_"


def marker() -> str:
    return f"{PREFIX}{uuid.uuid4().hex[:12]}"


def image(path: Path, fmt: str, mark: str) -> None:
    from PIL import Image, ImageDraw

    img = Image.new("RGB", (640, 480), (40, 90, 160))
    ImageDraw.Draw(img).text((20, 220), mark, fill=(255, 255, 255))
    exif = Image.Exif()
    exif[0x010E] = mark            # ImageDescription
    exif[0x0132] = FIXED_DATE      # DateTime
    exif.get_ifd(0x8769)[0x9003] = FIXED_DATE   # DateTimeOriginal
    if fmt == "HEIF":
        import pillow_heif
        pillow_heif.register_heif_opener()
    img.save(path, fmt, exif=exif.tobytes()) if fmt != "PNG" else img.save(path, fmt)


def movie(path: Path, mark: str) -> None:
    subprocess.run(
        ["ffmpeg", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
         "testsrc=size=640x480:rate=30:duration=2", "-pix_fmt", "yuv420p",
         "-metadata", "creation_time=2001-01-01T12:00:00Z",
         "-metadata", f"comment={mark}", str(path)],
        check=True,
    )


def main(out: Path) -> int:
    out.mkdir(parents=True, exist_ok=True)
    made, skipped = [], []
    jobs = [("jpg", "JPEG"), ("png", "PNG"), ("heic", "HEIF")]
    for ext, fmt in jobs:
        m = marker()
        p = out / f"{m}.{ext}"
        try:
            image(p, fmt, m)
            made.append({"file": p.name, "marker": m})
        except Exception as e:
            skipped.append({"format": ext, "reason": f"{type(e).__name__}: {e}"})
    if shutil.which("ffmpeg"):
        m = marker()
        movie(out / f"{m}.mov", m)
        made.append({"file": f"{m}.mov", "marker": m})
    else:
        skipped.append({"format": "mov", "reason": "ffmpeg not installed"})

    # Live photo = HEIC + MOV sharing one Apple ContentIdentifier.
    heic = next((x for x in made if x["file"].endswith(".heic")), None)
    if heic and shutil.which("ffmpeg") and shutil.which("exiftool"):
        cid = str(uuid.uuid4()).upper()
        m = marker()
        still, clip = out / f"{m}_live.heic", out / f"{m}_live.mov"
        image(still, "HEIF", m)
        movie(clip, m)
        subprocess.run(["exiftool", "-q", "-overwrite_original",
                        f"-ContentIdentifier={cid}", str(still)], check=True)
        subprocess.run(["exiftool", "-q", "-overwrite_original",
                        f"-QuickTime:ContentIdentifier={cid}", str(clip)], check=True)
        got = subprocess.run(["exiftool", "-s3", "-ContentIdentifier", str(still)],
                             capture_output=True, text=True).stdout.strip()
        if got == cid:
            made.append({"file": still.name, "pair": clip.name, "marker": m, "live": cid})
        else:
            # Apple keeps it in Apple MakerNotes, which exiftool cannot create.
            for f in (still, clip):
                f.unlink()
            skipped.append({"format": "live", "reason": "HEIC ContentIdentifier not "
                            "writable without Apple MakerNotes; use a real iPhone Live Photo"})
    else:
        skipped.append({"format": "live", "reason": "needs HEIC + ffmpeg + exiftool"})

    (out / "manifest.json").write_text(json.dumps({"made": made, "skipped": skipped},
                                                  ensure_ascii=False, indent=2))
    print(json.dumps({"made": len(made), "skipped": skipped}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main(Path(sys.argv[1] if len(sys.argv) > 1 else "test-media")))
