import pytest

from icloud_bridge.backend import BackendError
from icloud_bridge.pyicloud_backend import PhotosServiceException, PyiCloudBackend


class _Album:
    def __init__(self, id, name):
        self.id, self.name = id, name


class _Lib:
    def __init__(self, albums, raises=True):
        self.albums, self._raises = albums, raises

    def create_album(self, name):
        if self._raises:
            raise PhotosServiceException("Album creation outcome is uncertain; refresh before retrying")
        return _Album("NEW", name)


def _backend(lib):
    class Stub(PyiCloudBackend):               # subclass: the real class stays untouched
        library = property(lambda self: lib)   # no iCloud session

    be = object.__new__(Stub)
    be._albums, be._photos = {}, {}
    return be


def test_uncertain_create_is_confirmed_by_name_not_retried():
    be = _backend(_Lib([_Album("A1", "__bridge_test_x")]))
    rec = be.create_album("__bridge_test_x", None)
    assert rec.id == "A1" and be._albums["A1"].name == "__bridge_test_x"


def test_uncertain_create_with_no_album_fails_cleanly():
    be = _backend(_Lib([_Album("A1", "other")]))
    with pytest.raises(BackendError, match="not retried"):
        be.create_album("__bridge_test_x", None)


def test_uncertain_create_with_duplicate_names_is_refused():
    be = _backend(_Lib([_Album("A1", "__bridge_test_x"), _Album("A2", "__bridge_test_x")]))
    with pytest.raises(BackendError):
        be.create_album("__bridge_test_x", None)


def test_name_and_type_reads_tombstones_and_records():
    from types import SimpleNamespace as NS

    from icloud_bridge.pyicloud_backend import _name_and_type

    assert _name_and_type(NS(recordName="A")) == ("A", None)                      # tombstone
    assert _name_and_type(NS(recordName="B", recordType="CPLAsset")) == ("B", "CPLAsset")
    assert _name_and_type({"recordName": "C", "recordType": "CPLMaster"}) == ("C", "CPLMaster")


def test_raw_half_of_a_pair_is_its_own_read_only_entry():
    from datetime import datetime, timezone
    from types import SimpleNamespace as NS

    from icloud_bridge.backend import AssetRec
    from icloud_bridge.pyicloud_backend import PyiCloudBackend, _split_raw

    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    base = AssetRec(id="A1", filename="DSC0001.JPG", size=10, created=now, modified=now,
                    item_type="image")
    pair = NS(resources={"alternative": NS(filename="DSC0001.ARW", size=99)})
    raw = PyiCloudBackend._raw_rec(pair, base)
    assert (raw.id, raw.filename, raw.size) == ("A1~raw", "DSC0001.ARW", 99)
    # a plain JPEG/HEIC alternative or a photo without one is not a RAW
    assert PyiCloudBackend._raw_rec(NS(resources={"alternative": NS(filename="x.JPG", size=1)}), base) is None
    assert PyiCloudBackend._raw_rec(NS(resources={}), base) is None
    # downloads of the RAW entry read the alternative resource; thumbnails stay
    assert _split_raw("A1~raw", "original") == ("A1", "alternative")
    assert _split_raw("A1~raw", "thumb") == ("A1", "thumb")
    assert _split_raw("A1", "original") == ("A1", "original")
