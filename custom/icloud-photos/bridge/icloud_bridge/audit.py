"""Append-only JSONL audit log: one line per write attempt, success or not."""

from __future__ import annotations

import json
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


class Audit:
    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def write(self, op: str, args: dict[str, Any], *, before: Any = None,
              after: Any = None, result: Any = None, error: str | None = None,
              step: str | None = None) -> None:
        line = {
            "ts": datetime.now(timezone.utc).isoformat(),
            "op": op, "step": step, "args": args,
            "before": before, "after": after, "result": result, "error": error,
        }
        with self._lock, self.path.open("a", encoding="utf-8") as f:
            f.write(json.dumps(line, ensure_ascii=False, default=str) + "\n")

    def tail(self, n: int = 50) -> list[dict]:
        if not self.path.exists():
            return []
        lines = self.path.read_text(encoding="utf-8").splitlines()[-n:]
        return [json.loads(l) for l in lines]
