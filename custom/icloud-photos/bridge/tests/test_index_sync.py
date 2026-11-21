"""Regression tests for index sync fixes: placeholder/id swap conflicts and
RAW-half album memberships."""

from datetime import datetime, timezone

from icloud_bridge.backend import AlbumRec, AssetRec, Snapshot
from icloud_bridge.index import Index


def _rec(i):
    now = datetime.now(timezone.utc)
    return AssetRec(id=i, filename=f"{i}.jpg", size=10, created=now, modified=now)


def test_replace_asset_id_tolerates_existing_membership(tmp_path):
    ix = Index(tmp_path / "t.db")
    ix.put_asset(_rec("pending-1"))
    ix.put_asset(_rec("REAL"))
    ix.add_member("album:A", "pending-1")
    ix.add_member("album:A", "REAL")      # sync already wrote the new id
    ix.replace_asset_id("pending-1", _rec("REAL"))
    assert ix.asset("pending-1") is None
    assert sorted(ix.albums_of("REAL")) == ["album:A"]
    # placeholder's rows are gone, not left dangling
    assert ix.albums_of("pending-1") == []


def test_snapshot_backfills_raw_memberships(tmp_path):
    ix = Index(tmp_path / "t.db")
    snap = Snapshot(
        assets=[_rec("ABC"), _rec("ABC~raw")],
        albums=[AlbumRec(id="album:A", name="A", parent_id=None, is_folder=False)],
        memberships={"album:A": ["ABC"]},
        smart={},
    )
    ix.replace_snapshot(snap)
    assert sorted(ix.albums_of("ABC")) == ["album:A"]
    assert sorted(ix.albums_of("ABC~raw")) == ["album:A"]   # twin mirrors the base
