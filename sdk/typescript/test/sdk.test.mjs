// Runs the built SDK against a fake server. Live check: test/smoke.mjs.
import { test } from "node:test"
import assert from "node:assert/strict"
import { createServer } from "node:http"
import { Sandbox, Client, DawnbxError } from "../dist/index.js"

const sb1 = {
  id: "sb-abc123",
  image: "python:3.12-slim",
  status: "running",
  network: "internet",
  created: "2026-09-25T00:00:00Z",
  expires_at: null,
  restarted_at: null,
}

test("create, exec, files, errors, dispose", async () => {
  const seen = []
  let getTries = 0
  const files = new Map()
  const pathOf = (u) => new URL(u, "http://x").searchParams.get("path")
  const srv = createServer(async (req, res) => {
    let body = ""
    for await (const c of req) body += c
    seen.push(`${req.method} ${req.url} ${body}`)
    assert.equal(req.headers.authorization, "Bearer k")
    const json = (s, v) => {
      res.writeHead(s, { "Content-Type": "application/json" })
      res.end(JSON.stringify(v))
    }
    if (req.url === "/v1/sandboxes" && req.method === "POST")
      return json(200, sb1)
    if (req.url.endsWith("/exec"))
      return json(200, { exit_code: 3, stdout: "hi\n", stderr: "" })
    if (req.url.includes("/files") && req.method === "PUT") {
      files.set(pathOf(req.url), body)
      res.writeHead(204)
      return res.end()
    }
    if (req.url.includes("/files")) {
      if (!files.has(pathOf(req.url)))
        return json(404, {
          code: "file_not_found",
          message: pathOf(req.url) + ": no such file",
        })
      res.writeHead(200)
      return res.end(files.get(pathOf(req.url)))
    }
    if (req.url === "/v1/sandboxes/sb-abc123" && req.method === "GET") {
      if (++getTries < 3)
        return json(503, { code: "cluster_unavailable", message: "down" })
      return json(200, sb1)
    }
    if (req.method === "DELETE")
      return json(404, { code: "not_found", message: "gone" })
    json(410, {
      code: "expired",
      message: "sandbox expired",
      hint: "use ttl=null",
    })
  })
  await new Promise((r) => srv.listen(0, "127.0.0.1", r))
  const o = { url: `http://127.0.0.1:${srv.address().port}`, apiKey: "k" }
  try {
    {
      await using sb = await Sandbox.create({ ...o, ttl: null })
      assert.equal(sb.id, "sb-abc123")
      const r = await sb.exec("echo hi; exit 3")
      assert.deepEqual([r.exitCode, r.stdout], [3, "hi\n"])
      await sb.files.write("a/b.txt", "x")
      assert.equal(await sb.files.read("a/b.txt"), "x")
      await assert.rejects(
        sb.files.read("nope.txt"),
        (e) => e instanceof DawnbxError && e.code === "file_not_found",
      )
      await assert.rejects(
        sb.extend("1h"),
        (e) =>
          e instanceof DawnbxError &&
          e.code === "expired" &&
          e.hint === "use ttl=null",
      )
      const got = await Sandbox.get("sb-abc123", o) // retried through two 503s
      assert.equal(got.info.expiresAt, null)
    } // dispose -> DELETE; not_found is swallowed
    assert.match(seen[0], /POST \/v1\/sandboxes \{"ttl":null\}/)
    assert.ok(
      seen.some((s) =>
        s.startsWith("PUT /v1/sandboxes/sb-abc123/files?path=a%2Fb.txt x"),
      ),
    )
    assert.ok(seen.at(-1).startsWith("DELETE /v1/sandboxes/sb-abc123"))
    assert.equal(getTries, 3)
  } finally {
    srv.close()
  }
})

const STOPPED = { ...sb1, status: "stopped" }
const CHILD = { ...STOPPED, id: "sb-child1", parent: "sb-abc123" }

test("list, fork, start and refresh", async () => {
  const seen = []
  const srv = createServer(async (req, res) => {
    let body = ""
    for await (const c of req) body += c
    seen.push(`${req.method} ${req.url} ${body}`)
    const json = (s, v) => {
      res.writeHead(s, { "Content-Type": "application/json" })
      res.end(JSON.stringify(v))
    }
    if (req.url === "/v1/sandboxes" && req.method === "GET")
      return json(200, { sandboxes: [sb1, CHILD] })
    if (req.url.endsWith("/fork")) return json(200, { sandboxes: [CHILD] })
    if (req.url.endsWith("/start")) return json(200, sb1)
    json(200, STOPPED)
  })
  await new Promise((r) => srv.listen(0, "127.0.0.1", r))
  const o = { url: `http://127.0.0.1:${srv.address().port}`, apiKey: "k" }
  try {
    const all = await Sandbox.list(o)
    assert.deepEqual(
      all.map((s) => [s.id, s.parent]),
      [
        ["sb-abc123", undefined],
        ["sb-child1", "sb-abc123"],
      ],
    )
    assert.ok(all[0].created instanceof Date)

    const sb = await Sandbox.get("sb-abc123", o)
    const kids = await sb.fork(2, { ttl: null })
    assert.deepEqual(
      kids.map((k) => k.id),
      ["sb-child1"],
    )
    assert.equal(
      seen.at(-1),
      `POST /v1/sandboxes/sb-abc123/fork {"count":2,"ttl":null}`,
    )

    await sb.fork(1)
    assert.equal(seen.at(-1), `POST /v1/sandboxes/sb-abc123/fork {"count":1}`)

    await sb.start()
    assert.equal(sb.info.status, "running")
    const fresh = await sb.refresh()
    assert.equal(fresh.status, "stopped")
    assert.equal(sb.info.status, "stopped")
  } finally {
    srv.close()
  }
})

test("an unreachable server raises connection_failed", async () => {
  // Port 1 is reserved and never listening, so the client exhausts its retries.
  await assert.rejects(
    Sandbox.list({ url: "http://127.0.0.1:1", apiKey: "k" }),
    (e) => {
      assert.ok(e instanceof DawnbxError)
      assert.equal(e.code, "connection_failed")
      assert.match(e.message, /cannot reach/)
      assert.match(e.hint, /DAWNBX_URL/)
      return true
    },
  )
})

test("env defaults and create options", async () => {
  const seen = []
  const srv = createServer(async (req, res) => {
    let body = ""
    for await (const c of req) body += c
    seen.push(`${req.method} ${req.url} ${body}`)
    res.writeHead(200, { "Content-Type": "application/json" })
    res.end(
      JSON.stringify(req.url === "/v1/sandboxes" ? { sandboxes: [sb1] } : sb1),
    )
  })
  await new Promise((r) => srv.listen(0, "127.0.0.1", r))
  const prevUrl = process.env.DAWNBX_URL
  const prevKey = process.env.DAWNBX_API_KEY
  process.env.DAWNBX_URL = `http://127.0.0.1:${srv.address().port}`
  process.env.DAWNBX_API_KEY = "env-key"
  try {
    // No url/apiKey passed: both come from the environment.
    await Sandbox.list()
    assert.equal(seen.at(-1), "GET /v1/sandboxes ")

    await Sandbox.create({
      image: "python:3.12",
      ttl: "30m",
      network: "none",
      cpu: "2",
      memory: "2Gi",
    })
    assert.equal(
      seen.at(-1),
      `POST /v1/sandboxes {"image":"python:3.12","network":"none","cpu":"2","memory":"2Gi","ttl":"30m"}`,
    )

    // A bare create sends only the fields the caller set.
    await Sandbox.create()
    assert.equal(seen.at(-1), "POST /v1/sandboxes {}")
  } finally {
    if (prevUrl === undefined) delete process.env.DAWNBX_URL
    else process.env.DAWNBX_URL = prevUrl
    if (prevKey === undefined) delete process.env.DAWNBX_API_KEY
    else process.env.DAWNBX_API_KEY = prevKey
    srv.close()
  }
})

// A cluster still coming up: every optional field is absent, not empty.
const BARE = {
  name: "warm",
  provider: "aws",
  region: "us-east-1",
  instance_type: "t4g.small",
  disk_gib: 20,
  status: "provisioning",
  url: "",
}

const NODE = {
  id: "i-0abc",
  instance_type: "t4g.medium",
  status: "ready",
  sandboxes: 3,
  detail: "kubelet healthy",
}

const clusterServer = async (handler) => {
  const seen = []
  const srv = createServer(async (req, res) => {
    let body = ""
    for await (const c of req) body += c
    seen.push(`${req.method} ${req.url} ${body}`)
    const json = (s, v) => {
      res.writeHead(s, { "Content-Type": "application/json" })
      res.end(JSON.stringify(v))
    }
    await handler(req, res, json)
  })
  await new Promise((r) => srv.listen(0, "127.0.0.1", r))
  return {
    seen,
    client: new Client({
      url: `http://127.0.0.1:${srv.address().port}`,
      apiKey: "k",
    }),
    close: () => srv.close(),
  }
}

test("kill rethrows anything that is not an already-gone sandbox", async () => {
  const { client, close } = await clusterServer(async (req, res, json) => {
    if (req.method === "DELETE")
      return json(410, { code: "expired", message: "sandbox expired" })
    json(200, sb1)
  })
  try {
    const sb = await Sandbox.get("sb-abc123", {
      url: client.url,
      apiKey: "k",
    })
    // The server answers 410, not the 404 that disposal tolerates.
    await assert.rejects(sb.kill(), (e) => {
      assert.ok(e instanceof DawnbxError)
      assert.equal(e.code, "expired")
      return true
    })
  } finally {
    close()
  }
})

test("a Client with no API key raises unauthorized before any request", async () => {
  const { seen, client, close } = await clusterServer(async (req, res, json) =>
    json(200, { clusters: [] }),
  )
  const prevKey = process.env.DAWNBX_API_KEY
  delete process.env.DAWNBX_API_KEY
  try {
    assert.throws(
      () => new Client({ url: client.url }),
      (e) => {
        assert.ok(e instanceof DawnbxError)
        assert.equal(e.code, "unauthorized")
        assert.match(e.hint, /DAWNBX_API_KEY/)
        return true
      },
    )
    assert.deepEqual(seen, [])
  } finally {
    if (prevKey === undefined) delete process.env.DAWNBX_API_KEY
    else process.env.DAWNBX_API_KEY = prevKey
    close()
  }
})
