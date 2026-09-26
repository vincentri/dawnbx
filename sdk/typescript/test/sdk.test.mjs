// Runs the built SDK against a fake server. Live check: test/smoke.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { Sandbox, DawnbxError } from "../dist/index.js";

const sb1 = { id: "sb-abc123", image: "python:3.12-slim", status: "running", network: "internet",
  created: "2026-09-25T00:00:00Z", expires_at: null, restarted_at: null };

test("create, exec, files, errors, dispose", async () => {
  const seen = [];
  let getTries = 0;
  const files = new Map();
  const pathOf = (u) => new URL(u, "http://x").searchParams.get("path");
  const srv = createServer(async (req, res) => {
    let body = "";
    for await (const c of req) body += c;
    seen.push(`${req.method} ${req.url} ${body}`);
    assert.equal(req.headers.authorization, "Bearer k");
    const json = (s, v) => { res.writeHead(s, { "Content-Type": "application/json" }); res.end(JSON.stringify(v)); };
    if (req.url === "/v1/sandboxes" && req.method === "POST") return json(200, sb1);
    if (req.url.endsWith("/exec")) return json(200, { exit_code: 3, stdout: "hi\n", stderr: "" });
    if (req.url.includes("/files") && req.method === "PUT") { files.set(pathOf(req.url), body); res.writeHead(204); return res.end(); }
    if (req.url.includes("/files")) {
      if (!files.has(pathOf(req.url))) return json(404, { code: "file_not_found", message: pathOf(req.url) + ": no such file" });
      res.writeHead(200); return res.end(files.get(pathOf(req.url)));
    }
    if (req.url === "/v1/sandboxes/sb-abc123" && req.method === "GET") {
      if (++getTries < 3) return json(503, { code: "cluster_unavailable", message: "down" });
      return json(200, sb1);
    }
    if (req.method === "DELETE") return json(404, { code: "not_found", message: "gone" });
    json(410, { code: "expired", message: "sandbox expired", hint: "use ttl=null" });
  });
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  const o = { url: `http://127.0.0.1:${srv.address().port}`, apiKey: "k" };
  try {
    {
      await using sb = await Sandbox.create({ ...o, ttl: null });
      assert.equal(sb.id, "sb-abc123");
      const r = await sb.exec("echo hi; exit 3");
      assert.deepEqual([r.exitCode, r.stdout], [3, "hi\n"]);
      await sb.files.write("a/b.txt", "x");
      assert.equal(await sb.files.read("a/b.txt"), "x");
      await assert.rejects(sb.files.read("nope.txt"), (e) => e instanceof DawnbxError && e.code === "file_not_found");
      await assert.rejects(sb.extend("1h"), (e) => e instanceof DawnbxError && e.code === "expired" && e.hint === "use ttl=null");
      const got = await Sandbox.get("sb-abc123", o); // retried through two 503s
      assert.equal(got.info.expiresAt, null);
    } // dispose -> DELETE; not_found is swallowed
    assert.match(seen[0], /POST \/v1\/sandboxes \{"ttl":null\}/);
    assert.ok(seen.some((s) => s.startsWith("PUT /v1/sandboxes/sb-abc123/files?path=a%2Fb.txt x")));
    assert.ok(seen.at(-1).startsWith("DELETE /v1/sandboxes/sb-abc123"));
    assert.equal(getTries, 3);
  } finally {
    srv.close();
  }
});
