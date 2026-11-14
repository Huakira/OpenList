from fastapi.testclient import TestClient

from icloud_bridge.api import create_app
from icloud_bridge.backend import FakeBackend
from icloud_bridge.config import Settings

H = {"Authorization": "Bearer secret"}


def test_api_roundtrip(tmp_path):
    fb = FakeBackend()
    fb.seed_asset("a.jpg")
    app = create_app(Settings(token="secret", data_dir=tmp_path, apple_id="x"),
                     backend=fb, start_sync=False)
    with TestClient(app) as c:
        app.state.ops.refresh()
        assert c.get("/v1/list").status_code == 401
        assert c.get("/v1/health").json()["ok"]
        root = c.get("/v1/list", headers=H).json()["items"]
        assert root[0] == {**root[0], "id": "all", "is_dir": True}
        r = c.post("/v1/mkdir", json={"parent": "root", "name": "nope"}, headers=H)
        assert r.status_code == 403
        album = c.post("/v1/mkdir", json={"parent": "root", "name": "__bridge_test_x"},
                       headers=H).json()
        up = c.put("/v1/upload", params={"parent": album["id"], "name": "b.jpg"},
                   content=b"12345", headers=H)
        assert up.status_code == 200, up.text
        app.state.ops.wait_uploads()
        item = c.get("/v1/list", params={"id": album["id"]}, headers=H).json()["items"][0]
        assert item["name"] == "b.jpg" and item["size"] == 5
        link = c.get("/v1/link", params={"id": item["id"]}, headers=H).json()
        assert link["url"].startswith("https://") and "path" not in link
        r = c.post("/v1/other", json={"op": "nuke", "id": item["id"]}, headers=H)
        assert r.status_code == 400


def test_raw_streams_icloud_bytes_with_range(tmp_path, monkeypatch):
    import icloud_bridge.api as api_mod

    class Up:
        def __init__(self, status, body, headers):
            self.status_code, self._b, self.headers = status, body, headers
        def iter_content(self, n):
            yield self._b
        def close(self):
            pass

    seen = {}

    def fake_get(url, headers=None, stream=False, timeout=None):
        seen.update(url=url, headers=headers or {})
        if headers and "Range" in headers:
            return Up(206, b"234", {"Content-Range": "bytes 2-4/10", "Content-Length": "3",
                                    "Content-Type": "image/heic"})
        return Up(200, b"0123456789", {"Content-Length": "10", "Content-Type": "image/heic"})

    monkeypatch.setattr(api_mod.requests, "get", fake_get)
    fb = FakeBackend()
    a = fb.seed_asset("a.heic")
    app = create_app(Settings(token="secret", data_dir=tmp_path, apple_id="x"),
                     backend=fb, start_sync=False)
    with TestClient(app) as c:
        app.state.ops.refresh()
        r = c.get("/v1/raw", params={"id": f"f|all|{a.id}"}, headers=H)
        assert r.status_code == 200 and r.content == b"0123456789"
        assert seen["url"].startswith("https://cvws.icloud-content.com/")
        r = c.get("/v1/raw", params={"id": f"f|all|{a.id}"}, headers={**H, "Range": "bytes=2-4"})
        assert r.status_code == 206 and r.content == b"234"
        assert r.headers["content-range"] == "bytes 2-4/10"
        assert c.get("/v1/raw", params={"id": f"f|all|{a.id}"}).status_code == 401
