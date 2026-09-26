# Runs the SDK against a fake server. Live check: tests/smoke.py.
#   python -m unittest discover -s tests
import json
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import ClassVar
from urllib.parse import parse_qs, urlsplit

sys.path.insert(0, str(Path(__file__).parent.parent / "src"))
from dawnbx import DawnbxError, Sandbox

SB = {
    "id": "sb-abc123",
    "image": "python:3.12-slim",
    "status": "running",
    "network": "internet",
    "created": "2026-09-25T00:00:00.123456789Z",
    "expires_at": None,
    "restarted_at": None,
}


class Fake(BaseHTTPRequestHandler):
    seen: ClassVar[list] = []
    get_tries = 0
    files: ClassVar[dict] = {}

    def log_message(self, *a):
        pass

    def reply(self, status, body=None, raw=None):
        self.send_response(status)
        self.end_headers()
        if raw is not None:
            self.wfile.write(raw)
        elif body is not None:
            self.wfile.write(json.dumps(body).encode())

    def handle_any(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode()
        Fake.seen.append(f"{self.command} {self.path} {body}")
        assert self.headers["Authorization"] == "Bearer k"
        p = self.path
        if p == "/v1/sandboxes" and self.command == "POST":
            return self.reply(200, SB)
        if p.endswith("/exec"):
            return self.reply(200, {"exit_code": 3, "stdout": "hi\n", "stderr": ""})
        if "/files" in p and self.command == "PUT":
            Fake.files[parse_qs(urlsplit(p).query)["path"][0]] = body
            return self.reply(204)
        if "/files" in p:
            key = parse_qs(urlsplit(p).query)["path"][0]
            if key not in Fake.files:
                return self.reply(
                    404, {"code": "file_not_found", "message": f"{key}: no such file"}
                )
            return self.reply(200, raw=Fake.files[key].encode())
        if p == "/v1/sandboxes/sb-abc123" and self.command == "GET":
            Fake.get_tries += 1
            if Fake.get_tries < 3:
                return self.reply(503, {"code": "cluster_unavailable", "message": "down"})
            return self.reply(200, SB)
        if self.command == "DELETE":
            return self.reply(404, {"code": "not_found", "message": "gone"})
        self.reply(410, {"code": "expired", "message": "sandbox expired", "hint": "use ttl=None"})

    do_GET = do_POST = do_PUT = do_DELETE = handle_any


class TestSDK(unittest.TestCase):
    def setUp(self):
        Fake.seen, Fake.get_tries, Fake.files = [], 0, {}

    def test_flow(self):
        srv = ThreadingHTTPServer(("127.0.0.1", 0), Fake)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        o = {"url": f"http://127.0.0.1:{srv.server_port}", "api_key": "k"}
        try:
            with Sandbox.create(ttl=None, **o) as sb:
                self.assertEqual(sb.id, "sb-abc123")
                self.assertIsNone(sb.info.expires_at)
                self.assertEqual(sb.info.created.microsecond, 123456)
                r = sb.exec("echo hi; exit 3")
                self.assertEqual((r.exit_code, r.stdout), (3, "hi\n"))
                sb.files.write("a/b.txt", "x")
                self.assertEqual(sb.files.read("a/b.txt"), "x")
                with self.assertRaises(DawnbxError) as cm:
                    sb.files.read("nope.txt")
                self.assertEqual(cm.exception.code, "file_not_found")
                with self.assertRaises(DawnbxError) as cm:
                    sb.extend("1h")
                self.assertEqual(
                    (cm.exception.code, cm.exception.hint, cm.exception.status),
                    ("expired", "use ttl=None", 410),
                )
                Sandbox.get("sb-abc123", **o)  # retried through two 503s
            # exiting the block -> DELETE; not_found is swallowed
            self.assertIn('POST /v1/sandboxes {"ttl": null}', Fake.seen[0])
            self.assertTrue(
                any(
                    s.startswith("PUT /v1/sandboxes/sb-abc123/files?path=a%2Fb.txt x")
                    for s in Fake.seen
                )
            )
            self.assertTrue(Fake.seen[-1].startswith("DELETE /v1/sandboxes/sb-abc123"))
            self.assertEqual(Fake.get_tries, 3)
        finally:
            srv.shutdown()

    def test_no_key(self):
        with self.assertRaises(DawnbxError) as cm:
            Sandbox.create(api_key="", url="http://127.0.0.1:1")
        self.assertEqual(cm.exception.code, "unauthorized")


if __name__ == "__main__":
    unittest.main()
