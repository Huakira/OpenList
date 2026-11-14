"""icloud-bridge login | serve"""

from __future__ import annotations

import argparse
import getpass
import os
import sys

from . import config


def login(s: config.Settings) -> int:
    from pyicloud import PyiCloudService

    if not s.apple_id:
        print("set ICLOUD_APPLE_ID first", file=sys.stderr)
        return 2
    s.cookie_dir.mkdir(parents=True, exist_ok=True)
    password = os.environ.get("ICLOUD_PASSWORD") or getpass.getpass("Apple ID password: ")
    api = PyiCloudService(s.apple_id, password, cookie_directory=str(s.cookie_dir))
    if api.requires_2fa:
        code = input("2FA code: ").strip()
        if not api.validate_2fa_code(code):
            print("2FA code rejected", file=sys.stderr)
            return 1
        if not api.is_trusted_session:
            api.trust_session()
    elif api.requires_2sa:
        print("legacy 2SA accounts are not supported", file=sys.stderr)
        return 1
    print("session saved; trusted =", api.is_trusted_session)
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="icloud-bridge")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("login", help="password + 2FA, saves the session to the data volume")
    srv = sub.add_parser("serve")
    srv.add_argument("--host", default="0.0.0.0")
    srv.add_argument("--port", type=int, default=8080)
    args = ap.parse_args(argv)

    if args.cmd == "login":
        os.environ.setdefault("BRIDGE_TOKEN", "login-only")
        return login(config.load())
    import uvicorn
    from .api import create_app
    uvicorn.run(create_app(), host=args.host, port=args.port, log_level="info")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
