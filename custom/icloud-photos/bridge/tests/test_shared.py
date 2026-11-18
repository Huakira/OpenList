"""Shared photo streams (legacy 共享相册): listing, on-demand refresh,
read-only guards, and isolation from the private library."""

import io
from dataclasses import replace
from pathlib import Path

import pytest

from icloud_bridge.audit import Audit
from icloud_bridge.backend import FakeBackend
from icloud_bridge.config import Settings
from icloud_bridge.index import Index
from icloud_bridge.ops import Forbidden, NotFound, Ops, file_id

# fresh_max_age=0: every open re-reads that one scope (deterministic, the
# account is fake); fresh_wait=5: the open blocks until the re-read is done.
S = dict(token="t", apple_id="x", fresh_max_age=0.0, fresh_wait=5.0)


@pytest.fixture
def env(tmp_path: Path):
    s = Settings(data_dir=tmp_path, **S)
    fb = FakeBackend()
    priv = fb.seed_asset("IMG_0001.HEIC")
    alb = fb.seed_shared_album("曹泽宇to谢华可")
    a1 = fb.seed_shared_asset(alb.id, "IMG_6001.JPG")
    a2 = fb.seed_shared_asset(alb.id, "IMG_6002.HEIC")
    ops = Ops(s, fb, Index(s.db_path), Audit(s.audit_path))
    ops.refresh()
    ops.list("shared")                      # on-demand: this fills the index
    ops.list(f"album:{alb.id}")
    yield ops, fb, {"priv": priv, "alb": alb, "a1": a1, "a2": a2}
    ops.close()


def names(ops, key):
    return {e.name: e for e in ops.list(key)}


def test_shared_dir_at_root_and_albums(env):
    ops, fb, d = env
    root = names(ops, "root")
    assert "共享相册" in root and root["共享相册"].id == "shared"
    shared = names(ops, "shared")
    assert set(shared) == {"曹泽宇to谢华可"}
    assert shared["曹泽宇to谢华可"].id == f"album:{d['alb'].id}"
    files = [e.name for e in ops.list(f"album:{d['alb'].id}")]
    assert files == ["IMG_6001.JPG", "IMG_6002.HEIC"]
    # shared assets never leak into the private views
    assert [e.name for e in ops.list("all")] == ["IMG_0001.HEIC"]
    assert ops.index.get_meta("shared_last_sync") is not None


def test_shared_ids_never_collide_with_private(env):
    ops, fb, d = env
    assert d["alb"].id.startswith("s:") and d["a1"].id.startswith("s:")
    assert ops.index.album(d["alb"].id).parent_id == "shared"
    assert ops.index.child_albums(None) == []          # not at the private root


def test_every_write_to_shared_is_refused(env):
    ops, fb, d = env
    alb_key, fid = f"album:{d['alb'].id}", file_id(f"album:{d['alb'].id}", d["a1"].id)
    with pytest.raises(Forbidden):
        ops.remove(fid)                    # delete a shared photo
    with pytest.raises(Forbidden):
        ops.remove(alb_key)                # delete the shared album
    with pytest.raises(Forbidden):
        ops.rename(alb_key, "x")           # rename the shared album
    with pytest.raises(Forbidden):
        ops.mkdir("shared", "x")           # create an album inside shared
    with pytest.raises(Forbidden):
        ops.mkdir(alb_key, "x")            # ... or inside a shared album
    with pytest.raises(Forbidden):
        ops.move(fid, alb_key)             # move a shared photo around
    with pytest.raises(Forbidden):
        ops.move(file_id("all", d["priv"].id), alb_key)   # move into shared
    with pytest.raises(Forbidden):
        ops.copy(file_id("all", d["priv"].id), alb_key)
    with pytest.raises(Forbidden):
        ops.upload(alb_key, "x.jpg", io.BytesIO(b"1"))
    with pytest.raises(Forbidden):
        ops.other("favorite", fid)
    with pytest.raises(Forbidden):
        ops.other("hide", fid)
    assert fb.calls == []                  # nothing reached iCloud
    assert all(l["error"].startswith("refused") for l in ops.audit.tail())


def test_shared_download_link(env):
    ops, fb, d = env
    ops.list(f"album:{d['alb'].id}")
    fid = file_id(f"album:{d['alb'].id}", d["a1"].id)
    res = ops.link(fid)
    assert res["local"] is False and res["url"].startswith("https://")


def test_open_reads_and_replaces(env):
    ops, fb, d = env
    alb = d["alb"]
    # a new photo arrives upstream, another one leaves
    fb.seed_shared_asset(alb.id, "IMG_6003.MOV")
    fb.members[alb.id].remove(d["a2"].id)
    files = [e.name for e in ops.list(f"album:{alb.id}")]
    assert files == ["IMG_6001.JPG", "IMG_6003.MOV"]
    assert ops.index.asset(d["a2"].id) is None        # gone for good
    # the album itself is renamed upstream
    fb.albums[alb.id] = replace(fb.albums[alb.id], name="新名字")
    assert "新名字" in names(ops, "shared")
    # the album is deleted upstream
    fb.albums.pop(alb.id)
    fb.members.pop(alb.id)
    assert names(ops, "shared") == {}
    assert ops.index.album(alb.id) is None
    assert ops.index.asset(d["a1"].id) is None


def test_shared_failure_serves_stale_and_retries(env):
    ops, fb, d = env
    alb = d["alb"]
    first = [e.name for e in ops.list(f"album:{alb.id}")]
    fb.shared_fail.add(alb.id)
    assert [e.name for e in ops.list(f"album:{alb.id}")] == first   # stale, no raise
    fb.shared_fail.clear()
    fb.seed_shared_asset(alb.id, "IMG_6009.JPG")
    got = [e.name for e in ops.list(f"album:{alb.id}")]            # retry succeeded
    assert "IMG_6009.JPG" in got
    fb.shared_fail.add("list")
    assert names(ops, "shared")["曹泽宇to谢华可"].id == f"album:{alb.id}"


def test_private_full_snapshot_keeps_shared_rows(env):
    ops, fb, d = env
    ops.refresh(force=True)   # the fixture already opened shared once
    assert ops.index.album(d["alb"].id) is not None
    assert ops.index.asset(d["a1"].id) is not None
    fb.remove_asset(d["priv"].id)      # a private change still applies cleanly
    ops.refresh()
    assert ops.index.asset(d["priv"].id) is None
    assert ops.index.asset(d["a1"].id) is not None


def test_shared_can_be_hidden(env, tmp_path):
    ops, fb, d = env
    ops.s = replace(ops.s, show_shared_albums=False)
    assert "共享相册" not in names(ops, "root")
    with pytest.raises(NotFound):
        ops.list("shared")
    with pytest.raises(NotFound):
        ops.list(f"album:{d['alb'].id}")
    # ... and the private library is untouched by the setting
    assert [e.name for e in ops.list("all")] == ["IMG_0001.HEIC"]
