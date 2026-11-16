import io
from dataclasses import replace
from pathlib import Path

import pytest

from icloud_bridge.audit import Audit
from icloud_bridge.backend import FakeBackend
from icloud_bridge.config import Settings
from icloud_bridge.index import Index
from icloud_bridge.ops import Forbidden, Ops, ReadOnly, file_id

P = "__bridge_test_"


@pytest.fixture
def env(tmp_path: Path):
    s = Settings(token="t", data_dir=tmp_path, apple_id="x")
    fb = FakeBackend()
    # the user's real library: must never be touched while the whitelist is on
    real = fb.seed_asset("IMG_0001.HEIC")
    fb.seed_asset("IMG_0001.HEIC")                 # same name, different photo
    fav = fb.seed_asset("IMG_0002.JPG", favorite=True)
    trip = fb.seed_album("Trip 2024/25")
    fb.members[trip.id] = [real.id, fav.id]
    ops = Ops(s, fb, Index(s.db_path), Audit(s.audit_path))
    ops.refresh()
    yield ops, fb, {"real": real, "fav": fav, "trip": trip}
    ops.close()


def names(ops, key):
    return {e.name: e for e in ops.list(key)}


def writes(fb):
    return [c[0] for c in fb.calls]


def test_root_and_names(env):
    ops, fb, d = env
    root = names(ops, "root")
    assert {"全部照片", "收藏", "最近删除", "Trip 2024／25"} <= set(root)
    all_ = [e.name for e in ops.list("all")]
    assert all_[0] == "IMG_0001.HEIC"
    assert all_[1].startswith("IMG_0001~") and all_[1].endswith(".HEIC")
    assert "/" not in all_[1]
    assert [e.name for e in ops.list("all")] == all_            # stable


def test_whitelist_blocks_real_library(env):
    ops, fb, d = env
    fid = file_id(f"album:{d['trip'].id}", d["real"].id)
    with pytest.raises(Forbidden):
        ops.remove(fid)
    with pytest.raises(Forbidden):
        ops.remove(file_id("all", d["real"].id))
    with pytest.raises(Forbidden):
        ops.rename(f"album:{d['trip'].id}", P + "x")
    with pytest.raises(Forbidden):
        ops.mkdir("root", "no prefix")
    with pytest.raises(Forbidden):
        ops.other("hide", file_id("all", d["real"].id))
    assert writes(fb) == []                       # nothing reached iCloud
    log = ops.audit.tail()
    assert len(log) == 5 and all(l["error"].startswith("refused") for l in log)


def test_read_only_switch(env, tmp_path):
    ops, fb, d = env
    ops.s = replace(ops.s, read_only=True)
    with pytest.raises(ReadOnly):
        ops.mkdir("root", P + "a")
    with pytest.raises(ReadOnly):
        ops.upload("all", "x.jpg", io.BytesIO(b"1"))
    assert writes(fb) == []


def make_album_with_upload(ops, name="t1.jpg", data=b"abc"):
    album = ops.mkdir("root", P + "album")
    entry = ops.upload(album.id, name, io.BytesIO(data))
    return album, entry


def test_upload_placeholder_then_ready(env):
    ops, fb, d = env
    album = ops.mkdir("root", P + "album")
    gate = __import__("threading").Event()
    real_upload = fb.upload
    fb.upload = lambda p, n: (gate.wait(5), real_upload(p, n))[1]
    entry = ops.upload(album.id, "t1.jpg", io.BytesIO(b"abc"))
    assert entry.status == "processing"
    listed = names(ops, album.id)["t1.jpg"]
    assert listed.status == "processing"
    assert ops.link(listed.id)["local"] is True     # readable straight away
    gate.set()
    ops.wait_uploads()
    ready = names(ops, album.id)["t1.jpg"]
    assert ready.status == "ready" and not ready.id.endswith(entry.id.split("|")[2])
    asset_id = ready.id.split("|")[2]
    assert asset_id in fb.members[album.id.split(":")[1]]
    assert ops.index.is_owned("asset", asset_id)
    assert ops.link(ready.id)["url"].startswith("https://")


def test_remove_rules(env):
    ops, fb, d = env
    album, _ = make_album_with_upload(ops)
    ops.wait_uploads()
    f = names(ops, album.id)["t1.jpg"]
    aid = f.id.split("|")[2]
    ops.remove(f.id)                                   # album: archive + leave
    arch = ops._archive_id()
    assert aid in fb.members[arch] and aid not in fb.members[album.id.split(":")[1]]
    assert names(ops, "root")[P + "归档"]
    with pytest.raises(Forbidden):                     # archive: refused
        ops.remove(file_id(f"album:{arch}", aid))
    ops.remove(file_id("all", aid))                    # all: archive only, photo stays
    assert not fb.assets[aid].deleted


def test_move_adds_before_removing(env):
    ops, fb, d = env
    a, _ = make_album_with_upload(ops)
    b = ops.mkdir("root", P + "b")
    ops.wait_uploads()
    f = names(ops, a.id)["t1.jpg"]
    fb.calls.clear()
    ops.move(f.id, b.id)
    assert writes(fb) == ["add_to_album", "remove_from_album"]
    assert "t1.jpg" in names(ops, b.id) and "t1.jpg" not in names(ops, a.id)
    ops.copy(names(ops, b.id)["t1.jpg"].id, a.id)
    assert "t1.jpg" in names(ops, a.id) and "t1.jpg" in names(ops, b.id)


def test_remove_album_archives_first(env):
    ops, fb, d = env
    a, _ = make_album_with_upload(ops)
    ops.wait_uploads()
    aid = names(ops, a.id)["t1.jpg"].id.split("|")[2]
    fb.calls.clear()
    ops.remove(a.id)
    assert writes(fb)[-1] == "delete_album"
    assert aid in fb.members[ops._archive_id()]
    assert P + "album" not in names(ops, "root")


def test_overwrite_keeps_albums_and_flags(env):
    ops, fb, d = env
    a, _ = make_album_with_upload(ops, data=b"old")
    b = ops.mkdir("root", P + "b")
    ops.wait_uploads()
    old = names(ops, a.id)["t1.jpg"]
    old_id = old.id.split("|")[2]
    ops.copy(old.id, b.id)
    ops.other("favorite", old.id)
    ops.upload(a.id, "t1.jpg", io.BytesIO(b"new"))
    ops.wait_uploads()
    new = names(ops, a.id)["t1.jpg"]
    new_id = new.id.split("|")[2]
    assert new_id != old_id and fb.blobs[new_id] == b"new"
    assert new_id in fb.members[b.id.split(":")[1]]          # restored album
    assert fb.assets[new_id].favorite                        # restored flag
    assert old_id in fb.members[ops._archive_id()]           # old archived
    assert old_id not in fb.members[b.id.split(":")[1]]
    assert not fb.assets[old_id].deleted                     # never really deleted


def test_overwrite_failure_keeps_scene(env):
    ops, fb, d = env
    a, _ = make_album_with_upload(ops, data=b"old")
    ops.wait_uploads()
    old_id = names(ops, a.id)["t1.jpg"].id.split("|")[2]
    fb.fail_on.add("upload")
    ph = ops.upload(a.id, "t1.jpg", io.BytesIO(b"new"))
    ops.wait_uploads()
    pid = ph.id.split("|")[2]
    assert ops.index.asset_status(pid)[0] == "failed"
    assert (ops.s.staging_dir / pid).exists()                 # staged copy kept
    assert old_id in fb.members[a.id.split(":")[1]]           # old untouched
    assert any(l["step"] == "b:upload" and l["error"] for l in ops.audit.tail())


def test_recover_by_moving_out_of_deleted(env):
    ops, fb, d = env
    a, _ = make_album_with_upload(ops)
    ops.wait_uploads()
    f = names(ops, a.id)["t1.jpg"]
    ops.other("purge", f.id)
    gone = names(ops, "deleted")["t1.jpg"]
    ops.move(gone.id, a.id)
    assert not fb.assets[gone.id.split("|")[2]].deleted


def test_cleanup_only_owned(env):
    ops, fb, d = env
    make_album_with_upload(ops)
    ops.wait_uploads()
    prev = ops.cleanup(confirm=False)["preview"]
    assert len(prev["albums"]) == 1 and len(prev["assets"]) == 1
    assert writes(fb).count("delete_album") == 0
    ops.cleanup(confirm=True)
    assert d["trip"].id in fb.albums and not fb.assets[d["real"].id].deleted
    assert fb.assets[prev["assets"][0]["id"]].deleted


def test_refresh_skips_when_unchanged(env):
    ops, fb, d = env
    n = fb.snapshots
    assert ops.refresh() is False and fb.snapshots == n         # nothing changed
    fb.seed_asset("new.jpg")                                    # change on iCloud
    assert ops.refresh() is True and fb.snapshots == n + 1
    assert "new.jpg" in names(ops, "all")
    assert ops.refresh(force=True) is True                      # manual refresh


def test_live_photo_is_read_only_even_without_whitelist(env):
    ops, fb, d = env
    live = fb.seed_asset("IMG_0100.HEIC", live=True)
    ops.refresh(force=True)
    ops.s = replace(ops.s, whitelist=False)        # simulate M5: whitelist off
    fid = file_id("all", live.id)
    trip = f"album:{d['trip'].id}"
    for call in (
        lambda: ops.remove(fid),
        lambda: ops.move(fid, trip),
        lambda: ops.copy(fid, trip),
        lambda: ops.other("favorite", fid),
        lambda: ops.other("purge", fid),
        lambda: ops.upload("all", "IMG_0100.HEIC", io.BytesIO(b"x")),
    ):
        with pytest.raises(Forbidden):
            call()
    assert fb.assets[live.id].favorite is False and not fb.assets[live.id].deleted
    # ordinary photos are still writable once the whitelist is off
    ops.other("favorite", file_id("all", d["real"].id))


def test_refresh_warms_a_restarted_backend(env):
    """A fresh process has a fresh index but no in-memory assets: an unchanged
    token must still warm them, or downloads 404 until the library changes."""
    ops, fb, d = env
    warmed: list[str] = []
    fb._ensure_loaded = lambda why="": warmed.append(why)      # type: ignore[attr-defined]
    assert ops.refresh() is False
    assert warmed == ["refresh"]
    fb.seed_asset("new.jpg")                                    # real change: snapshot path
    assert ops.refresh() is True and warmed == ["refresh"]


def test_same_name_albums_are_all_listed(env):
    ops, fb, d = env
    twin = fb.seed_album("Trip 2024/25")           # second album with the same name
    ops.refresh()
    ops.index.replace_snapshot(fb.snapshot())
    root = [e.name for e in ops.list("root")]
    trips = [n for n in root if n.startswith("Trip 2024")]
    assert len(trips) == 2 and len(set(trips)) == 2
    assert any("~" in n for n in trips)
    assert [e.name for e in ops.list("root")] == root       # stable
