"""dawnbx Python SDK. Zero dependencies; Python 3.9+.

with Sandbox.create() as sb:
    r = sb.exec("python -c 'print(1+1)'")
    print(r.stdout)
"""

from __future__ import annotations

import builtins
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime
from typing import Any

__all__ = ["DawnbxError", "ExecResult", "Sandbox", "SandboxInfo"]

_UNSET: Any = object()  # distinguishes "not passed" from None (= keep forever / no limit)


class DawnbxError(Exception):
    """Every API failure. `code` is stable; `hint` says what to do."""

    def __init__(self, code: str, message: str, hint: str | None = None, status: int | None = None):
        super().__init__(f"{message} ({hint})" if hint else message)
        self.code, self.hint, self.status = code, hint, status


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str
    pid: int | None = None  # set for background execs
    log: str | None = None


@dataclass
class SandboxInfo:
    id: str
    image: str
    status: str
    network: str
    created: datetime
    expires_at: datetime | None
    # Set when the sandbox was recreated (node reboot); processes did not survive, files did.
    restarted_at: datetime | None
    reason: str | None = None
    parent: str | None = None
    warnings: list[str] | None = None


def _date(s: str | None) -> datetime | None:
    # fromisoformat before 3.11 rejects "Z" and nanoseconds.
    if not s:
        return None
    s = s.replace("Z", "+00:00")
    if "." in s:
        head, rest = s.split(".", 1)
        frac, tz = (rest[: rest.index("+")], rest[rest.index("+") :]) if "+" in rest else (rest, "")
        s = f"{head}.{frac[:6]}{tz}"
    return datetime.fromisoformat(s)


def _info(v: dict[str, Any]) -> SandboxInfo:
    return SandboxInfo(
        id=v["id"],
        image=v["image"],
        status=v["status"],
        network=v["network"],
        created=_date(v["created"]),
        expires_at=_date(v.get("expires_at")),
        restarted_at=_date(v.get("restarted_at")),
        reason=v.get("reason"),
        parent=v.get("parent"),
        warnings=v.get("warnings"),
    )


class _Client:
    def __init__(self, url: str | None = None, api_key: str | None = None):
        self.url = (url or os.environ.get("DAWNBX_URL") or "http://127.0.0.1:8080").rstrip("/")
        self.key = api_key or os.environ.get("DAWNBX_API_KEY")
        if not self.key:
            raise DawnbxError(
                "unauthorized", "no API key", "set DAWNBX_API_KEY to the key the installer printed"
            )

    def req(
        self,
        method: str,
        path: str,
        body: Any = _UNSET,
        raw: bool = False,
        timeout: float | None = None,
    ) -> Any:
        headers = {"Authorization": f"Bearer {self.key}"}
        data = None
        if isinstance(body, (bytes, str)):
            data = body.encode() if isinstance(body, str) else body
        elif body is not _UNSET:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        # Reads are safe to repeat while the node restarts (cluster_unavailable).
        tries = 3 if method == "GET" else 1
        for i in range(1, tries + 1):
            r = urllib.request.Request(self.url + path, data=data, method=method, headers=headers)
            try:
                with urllib.request.urlopen(r, timeout=timeout) as res:
                    b = res.read()
                    if raw:
                        return b
                    return json.loads(b) if b else None
            except urllib.error.HTTPError as e:
                try:
                    err = json.loads(e.read())
                except ValueError:
                    err = {"code": f"http_{e.code}", "message": e.reason}
                if err.get("code") == "cluster_unavailable" and i < tries:
                    time.sleep(i)
                    continue
                raise DawnbxError(
                    err.get("code", f"http_{e.code}"),
                    err.get("message", ""),
                    err.get("hint"),
                    e.code,
                ) from None
            except (urllib.error.URLError, OSError) as e:
                if i < tries:
                    time.sleep(i)
                    continue
                raise DawnbxError(
                    "connection_failed",
                    f"cannot reach {self.url}: {e}",
                    "check DAWNBX_URL and that the server is running (systemctl status dawnbx)",
                ) from None


class Files:
    def __init__(self, c: _Client, id: str):
        self._c, self._id = c, id

    def _path(self, p: str) -> str:
        return f"/v1/sandboxes/{self._id}/files?path={urllib.parse.quote(p, safe='')}"

    def read(self, path: str) -> str:
        """Read a file as UTF-8. Relative paths are under /workspace."""
        return self.read_bytes(path).decode()

    def read_bytes(self, path: str) -> bytes:
        return self._c.req("GET", self._path(path), raw=True)

    def write(self, path: str, data: str | bytes) -> None:
        """Write a file, creating parent directories."""
        self._c.req("PUT", self._path(path), data)


class Sandbox:
    def __init__(self, c: _Client, info: SandboxInfo):
        self._c, self.info = c, info
        self.files = Files(c, info.id)

    @property
    def id(self) -> str:
        return self.info.id

    def __repr__(self) -> str:
        return f"Sandbox({self.id!r}, {self.info.status})"

    @classmethod
    def create(
        cls,
        image: str | None = None,
        ttl: str | None = _UNSET,
        network: str | None = None,
        cpu: str | None = None,
        memory: str | None = None,
        url: str | None = None,
        api_key: str | None = None,
    ) -> Sandbox:
        """Create a sandbox and wait until it can run commands.

        ttl: "30m", "2h"; None keeps it until killed. Default "1h".
        network: "internet" (default) or "none".
        """
        c = _Client(url, api_key)
        body = {
            k: v
            for k, v in {"image": image, "network": network, "cpu": cpu, "memory": memory}.items()
            if v is not None
        }
        if ttl is not _UNSET:
            body["ttl"] = ttl  # None is sent as null: keep until killed
        return cls(c, _info(c.req("POST", "/v1/sandboxes", body)))

    @classmethod
    def get(cls, id: str, url: str | None = None, api_key: str | None = None) -> Sandbox:
        c = _Client(url, api_key)
        return cls(c, _info(c.req("GET", f"/v1/sandboxes/{urllib.parse.quote(id, safe='')}")))

    @staticmethod
    def list(url: str | None = None, api_key: str | None = None) -> builtins.list[SandboxInfo]:
        return [_info(v) for v in _Client(url, api_key).req("GET", "/v1/sandboxes")["sandboxes"]]

    def exec(
        self,
        cmd: str,
        timeout: float | None = _UNSET,
        background: bool = False,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        """Run a shell command. A nonzero exit code is returned, not raised.

        timeout: seconds; None = no limit. Default 600.
        background: start and return at once; output goes to `log` inside the sandbox.
        """
        body: dict[str, Any] = {"cmd": cmd}
        if timeout is not _UNSET:
            body["timeout"] = timeout
        if background:
            body["background"] = True
        if env:
            body["env"] = env
        r = self._c.req("POST", f"/v1/sandboxes/{self.id}/exec", body)
        return ExecResult(r["exit_code"], r["stdout"], r["stderr"], r.get("pid"), r.get("log"))

    def fork(self, count: int = 1, ttl: str | None = _UNSET) -> builtins.list[Sandbox]:
        """Copy /workspace into `count` new sandboxes (max 10).

        Running processes are paused during the copy; children start with fresh
        processes. ttl works like create().
        """
        body: dict[str, Any] = {"count": count}
        if ttl is not _UNSET:
            body["ttl"] = ttl
        r = self._c.req("POST", f"/v1/sandboxes/{self.id}/fork", body)
        return [Sandbox(self._c, _info(v)) for v in r["sandboxes"]]

    def extend(self, ttl: str | None) -> None:
        """New TTL from now ("2h"), or None to keep until killed."""
        self.info = _info(self._c.req("POST", f"/v1/sandboxes/{self.id}/extend", {"ttl": ttl}))

    def start(self) -> None:
        """Restart a stopped sandbox (files kept)."""
        self.info = _info(self._c.req("POST", f"/v1/sandboxes/{self.id}/start"))

    def refresh(self) -> SandboxInfo:
        self.info = _info(self._c.req("GET", f"/v1/sandboxes/{self.id}"))
        return self.info

    def kill(self) -> None:
        """Delete the sandbox and its files."""
        try:
            self._c.req("DELETE", f"/v1/sandboxes/{self.id}")
        except DawnbxError as e:
            if e.code != "not_found":
                raise

    def __enter__(self) -> Sandbox:
        return self

    def __exit__(self, *exc: Any) -> None:
        self.kill()
