"""Password + 2FA login, driven from OpenList's storage form.

The two steps have to be spread over two saves: the first one sends the 2FA
prompt (Apple pushes it to the trusted device, or texts a code), the second one
submits the code the owner typed. The half-finished PyiCloudService therefore
lives in memory here, which is why the bridge process is kept running between
the two saves.
"""

from __future__ import annotations

import logging
import threading
from pathlib import Path

from .backend import BackendError

log = logging.getLogger(__name__)


class LoginState:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._pending: tuple[str, object] | None = None

    def start(self, apple_id: str, password: str, cookie_dir: Path, china: bool) -> bool:
        """Sign in. Returns True when the session is ready, False when a 2FA code is needed."""
        from pyicloud import PyiCloudService
        from pyicloud.exceptions import PyiCloudException

        if not apple_id.strip():
            raise BackendError("Apple ID is empty")
        if not password:
            raise BackendError("password is empty")
        cookie_dir.mkdir(parents=True, exist_ok=True)
        try:
            api = PyiCloudService(
                apple_id.strip(),
                password,
                cookie_directory=str(cookie_dir),
                china_mainland=china,
            )
        except PyiCloudException as e:
            raise BackendError(f"Apple ID or password rejected: {e}") from e
        if api.requires_2sa:
            raise BackendError("accounts with legacy two-step authentication are not supported")
        if api.requires_2fa:
            try:
                api.request_2fa_code()
            except Exception:  # the prompt usually went out during sign-in
                log.exception("could not request the 2FA code")
            with self._lock:
                self._pending = (apple_id.strip(), api)
            return False
        self._trust(api)
        with self._lock:
            self._pending = None
        return True

    def submit(self, apple_id: str, code: str) -> None:
        """Second step: verify the 2FA code for the pending sign-in."""
        with self._lock:
            pending = self._pending
        if pending is None or pending[0] != apple_id.strip():
            raise BackendError(
                "no sign-in is waiting for a code: fill in the Apple ID and password and save again"
            )
        api = pending[1]
        if not code.strip():
            raise BackendError("verification code is empty")
        try:
            ok = api.validate_2fa_code(code.strip())
        except Exception as e:
            raise BackendError(f"2FA verification failed: {e}") from e
        if not ok:
            raise BackendError("2FA code rejected")
        self._trust(api)
        with self._lock:
            self._pending = None

    def pending_for(self) -> str | None:
        with self._lock:
            return self._pending[0] if self._pending else None

    @staticmethod
    def _trust(api) -> None:
        try:
            if not api.is_trusted_session:
                api.trust_session()
        except Exception:
            # Not fatal: the session cookie is still saved and works until asked again.
            log.exception("trust_session failed")
