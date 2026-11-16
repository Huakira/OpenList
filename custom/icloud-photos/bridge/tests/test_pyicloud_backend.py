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
