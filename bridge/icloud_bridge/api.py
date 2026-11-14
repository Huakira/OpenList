"""JSON API for the Go driver (plan 3.3). Internal network only, token-guarded."""

from __future__ import annotations

import hmac
import logging
import tempfile
import threading
from contextlib import asynccontextmanager
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, Request
from fastapi.responses import FileResponse, JSONResponse, StreamingResponse
import requests
from pydantic import BaseModel
from starlette.background import BackgroundTask
from starlette.concurrency import run_in_threadpool

from . import config
from .audit import Audit
from .backend import BackendError, FakeBackend, PhotosBackend
from .index import Index
from .ops import NotFound, OpError, Ops

log = logging.getLogger("icloud_bridge")


class MkdirBody(BaseModel):
    parent: str
    name: str


class RenameBody(BaseModel):
    id: str
    name: str


class MoveBody(BaseModel):
    id: str
    to: str


class RemoveBody(BaseModel):
    id: str


class OtherBody(BaseModel):
    op: str
    id: str


class CleanupBody(BaseModel):
    confirm: bool = False


def make_backend(s: config.Settings) -> PhotosBackend:
    if s.backend == "fake":
        return FakeBackend()
    from .pyicloud_backend import PyiCloudBackend
    return PyiCloudBackend(s.apple_id, s.cookie_dir)


def create_app(settings: config.Settings | None = None,
               backend: PhotosBackend | None = None, start_sync: bool = True) -> FastAPI:
    s = settings or config.load()
    s.data_dir.mkdir(parents=True, exist_ok=True)
    state: dict[str, Any] = {"error": None}

    def build_ops() -> Ops:
        return Ops(s, backend or make_backend(s), Index(s.db_path), Audit(s.audit_path))

    def sync_loop(ops: Ops, stop: threading.Event) -> None:
        while True:
            try:
                ops.refresh()
                state["error"] = None
            except Exception as e:   # surfaced by /v1/health
                state["error"] = f"{type(e).__name__}: {e}"
                log.exception("refresh failed")
            if stop.wait(s.sync_interval):
                return

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        stop = threading.Event()
        try:
            app.state.ops = build_ops()
        except Exception as e:  # e.g. session needs 2FA: stay up and report it
            app.state.ops = None
            state["error"] = f"{type(e).__name__}: {e}"
        if app.state.ops is not None and start_sync:
            threading.Thread(target=sync_loop, args=(app.state.ops, stop),
                             daemon=True).start()
        yield
        stop.set()
        if app.state.ops is not None:
            app.state.ops.close()

    app = FastAPI(title="icloud-photos-bridge", lifespan=lifespan)

    def auth(authorization: str = Header(default="")) -> None:
        if not hmac.compare_digest(authorization, f"Bearer {s.token}"):
            raise HTTPException(401, "bad token")

    def ops_of(request: Request) -> Ops:
        ops = request.app.state.ops
        if ops is None:
            raise HTTPException(503, state["error"] or "backend unavailable")
        return ops

    @app.exception_handler(OpError)
    async def op_error(_: Request, e: OpError):
        return JSONResponse({"error": str(e), "kind": type(e).__name__}, status_code=e.status)

    @app.exception_handler(BackendError)
    async def backend_error(_: Request, e: BackendError):
        return JSONResponse({"error": str(e), "kind": "BackendError"}, status_code=502)

    guard = [Depends(auth)]

    @app.get("/v1/health")
    def health(request: Request):
        ops = request.app.state.ops
        body = {"ok": ops is not None and state["error"] is None, "error": state["error"],
                "read_only": s.read_only, "whitelist": s.whitelist}
        if ops is not None:
            body["last_sync"] = ops.index.get_meta("last_sync")
            try:
                body.update(ops.backend.health())
            except Exception as e:
                body.update(ok=False, error=f"{type(e).__name__}: {e}")
        return JSONResponse(body, status_code=200 if body["ok"] else 503)

    @app.get("/v1/list", dependencies=guard)
    def list_(request: Request, id: str = "root"):
        return {"items": [e.as_dict() for e in ops_of(request).list(id)]}

    @app.get("/v1/link", dependencies=guard)
    def link(request: Request, id: str, version: str = "original"):
        res = ops_of(request).link(id, version)
        res.pop("path", None)        # the driver fetches staged bytes via /v1/raw
        return res

    @app.get("/v1/thumb", dependencies=guard)
    def thumb(request: Request, id: str):
        return ops_of(request).link(id, "thumb")

    @app.get("/v1/raw", dependencies=guard)
    def raw(request: Request, id: str, version: str = "original"):
        """File bytes through the bridge, so the data path uses the bridge's egress
        (the hkdis tunnel) instead of a client-side 302 to iCloud. Range supported."""
        res = ops_of(request).link(id, version)
        if res.get("local"):
            return FileResponse(res["path"])   # staged upload; supports Range
        hdr = {}
        if request.headers.get("range"):
            hdr["Range"] = request.headers["range"]
        up = requests.get(res["url"], headers=hdr, stream=True, timeout=(20, 60))
        if up.status_code >= 400:
            up.close()
            raise HTTPException(status_code=502 if up.status_code >= 500 else up.status_code,
                                detail=f"iCloud returned {up.status_code}")
        keep = {k: v for k, v in up.headers.items()
                if k.lower() in ("content-type", "content-length", "content-range", "accept-ranges")}
        return StreamingResponse(up.iter_content(1 << 20), status_code=up.status_code,
                                 headers=keep, background=BackgroundTask(up.close))

    @app.post("/v1/mkdir", dependencies=guard)
    def mkdir(request: Request, b: MkdirBody):
        return ops_of(request).mkdir(b.parent, b.name).as_dict()

    @app.post("/v1/rename", dependencies=guard)
    def rename(request: Request, b: RenameBody):
        return ops_of(request).rename(b.id, b.name).as_dict()

    @app.post("/v1/move", dependencies=guard)
    def move(request: Request, b: MoveBody):
        return ops_of(request).move(b.id, b.to)

    @app.post("/v1/copy", dependencies=guard)
    def copy(request: Request, b: MoveBody):
        return ops_of(request).copy(b.id, b.to)

    @app.post("/v1/remove", dependencies=guard)
    def remove(request: Request, b: RemoveBody):
        return ops_of(request).remove(b.id)

    @app.post("/v1/other", dependencies=guard)
    def other(request: Request, b: OtherBody):
        return ops_of(request).other(b.op, b.id)

    @app.put("/v1/upload", dependencies=guard)
    async def upload(request: Request, parent: str, name: str):
        ops = ops_of(request)
        with tempfile.TemporaryFile(dir=s.staging_dir) as tmp:
            async for chunk in request.stream():
                tmp.write(chunk)
            tmp.seek(0)
            entry = await run_in_threadpool(ops.upload, parent, name, tmp)
        return entry.as_dict()

    @app.post("/v1/refresh", dependencies=guard)
    def refresh(request: Request):
        took = ops_of(request).refresh(force=True)
        return {"snapshot": took, "last_sync": ops_of(request).index.get_meta("last_sync")}

    @app.get("/v1/audit", dependencies=guard)
    def audit(request: Request, n: int = 50):
        return {"items": ops_of(request).audit.tail(n)}

    @app.get("/v1/cleanup", dependencies=guard)
    def cleanup_preview(request: Request):
        return ops_of(request).cleanup(confirm=False)

    @app.post("/v1/cleanup", dependencies=guard)
    def cleanup(request: Request, b: CleanupBody):
        return ops_of(request).cleanup(confirm=b.confirm)

    return app
