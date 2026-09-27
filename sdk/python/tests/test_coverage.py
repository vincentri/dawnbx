# Covers the SDK paths the main suite does not reach: list, fork, start,
# refresh, repr, the exec timeout field, and the two error paths that only fire
# when the server is unreachable or answers with a non-JSON body.
#   python -m unittest discover -s tests
import json
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import ClassVar

sys.path.insert(0, str(Path(__file__).parent.parent / "src"))
from dawnbx import DawnbxError, Sandbox

STOPPED = {
    "id": "sb-abc123",
    "image": "python:3.12-slim",
    "status": "stopped",
    "network": "internet",
    "created": "2026-09-25T00:00:00Z",
    "expires_at": None,
    "restarted_at": None,
}
RUNNING = dict(STOPPED, status="running")
CHILD = dict(RUNNING, id="sb-child1", parent="sb-abc123", status="stopped")


class Handler(BaseHTTPRequestHandler):
    seen: ClassVar[list] = []

    def log_message(self, *a):
        pass

    def reply(self, status, body=None, raw=None):
        self.send_response(status)
        if raw is not None:
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(raw)
            return
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        if body is not None:
            self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        Handler.seen.append(f"GET {self.path}")
        if self.path == "/v1/sandboxes":
            return self.reply(200, {"sandboxes": [RUNNING, CHILD]})
        if "/files" in self.path:
            return self.reply(502, raw=b"<html>bad gateway</html>")
        self.reply(200, STOPPED)

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        body = self.rfile.read(n).decode() if n else ""
        Handler.seen.append(f"POST {self.path} {body}")
        if self.path.endswith("/fork"):
            return self.reply(200, {"sandboxes": [CHILD]})
        if self.path.endswith("/start"):
            return self.reply(200, RUNNING)
        self.reply(400, {"code": "invalid_request", "message": "bad request"})

    do_PUT = do_DELETE = do_POST


class TestSdkSurface(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.opts = {"url": f"http://127.0.0.1:{cls.srv.server_port}", "api_key": "k"}

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    # Built through the public API, not by reaching into _Client and _info to
    # hand-assemble the object: the fake already answers GET /v1/sandboxes/{id}
    # with STOPPED, so the SDK can do what a caller would do. Reaching past the
    # public surface coupled this suite to the constructor, which the contract
    # does not promise and a refactor is free to change.
    def sandbox(self):
        return Sandbox.get("sb-abc123", **self.opts)

    def test_list_returns_every_sandbox(self):
        out = Sandbox.list(**self.opts)
        self.assertEqual([s.id for s in out], ["sb-abc123", "sb-child1"])
        self.assertEqual(out[1].parent, "sb-abc123")

    def test_fork_sends_count_and_explicit_ttl(self):
        kids = self.sandbox().fork(2, ttl=None)
        self.assertEqual([k.id for k in kids], ["sb-child1"])
        self.assertEqual(
            Handler.seen[-1], 'POST /v1/sandboxes/sb-abc123/fork {"count": 2, "ttl": null}'
        )

    def test_fork_omits_ttl_when_unset(self):
        self.sandbox().fork(1)
        self.assertEqual(Handler.seen[-1], 'POST /v1/sandboxes/sb-abc123/fork {"count": 1}')

    def test_start_then_refresh_track_status(self):
        sb = self.sandbox()
        self.assertEqual(sb.info.status, "stopped")
        sb.start()
        self.assertEqual(sb.info.status, "running")
        self.assertEqual(sb.refresh().status, "stopped")

    def test_repr_names_the_sandbox(self):
        self.assertEqual(repr(self.sandbox()), "Sandbox('sb-abc123', stopped)")

    def test_exec_sends_the_timeout_field(self):
        with self.assertRaises(DawnbxError) as cm:
            self.sandbox().exec("sleep 1", timeout="5s")
        self.assertEqual(cm.exception.code, "invalid_request")
        self.assertIn('"timeout": "5s"', Handler.seen[-1])

    def test_non_json_error_body_becomes_an_http_error(self):
        with self.assertRaises(DawnbxError) as cm:
            self.sandbox().files.read("anything")
        self.assertEqual(cm.exception.code, "http_502")
        self.assertEqual(cm.exception.status, 502)

    def test_unreachable_server_raises_connection_failed(self):
        # Port 1 is reserved and never listening, so the client exhausts retries.
        with self.assertRaises(DawnbxError) as cm:
            Sandbox.list(url="http://127.0.0.1:1", api_key="k")
        self.assertEqual(cm.exception.code, "connection_failed")
        self.assertIn("cannot reach", str(cm.exception))
        self.assertIn("DAWNBX_URL", cm.exception.hint or "")
