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
