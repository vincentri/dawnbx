// Runs the built SDK against a fake server. Live check: test/smoke.mjs.
import { test } from "node:test"
import assert from "node:assert/strict"
import { createServer } from "node:http"
import {
  Sandbox,
  Client,
  DawnbxError,
  listClusters,
  getCluster,
  listClusterNodes,
} from "../dist/index.js"

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

const CLUSTER = {
  name: "probe",
  provider: "aws",
  region: "eu-west-1",
  instance_type: "t4g.medium",
  disk_gib: 30,
  status: "ready",
  url: "https://probe.example",
  phase: "ready",
  detail: "1 node",
  hourly_usd: 0.0168,
  monthly_usd: 12.26,
  tls_pin: "sha256:AAAA",
}

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

test("listClusters returns the clusters array as the server sent it", async () => {
  const { seen, client, close } = await clusterServer(
    async (req, res, json) => {
      if (req.url === "/v1/clusters")
        return json(200, { clusters: [CLUSTER, BARE] })
      json(404, { code: "not_found", message: "no such route" })
    },
  )
  try {
    const all = await listClusters(client)
    assert.deepEqual(all, [CLUSTER, BARE])
    // Optional fields the server omitted stay absent, as Cluster declares them
    // optional: nothing is defaulted onto the object.
    assert.equal(BARE.tls_pin, undefined)
    assert.equal("tls_pin" in all[1], false)
    assert.equal(all[1].phase, undefined)
    assert.equal(all[0].tls_pin, "sha256:AAAA")
    assert.equal(all[0].hourly_usd, 0.0168)
    assert.equal(seen.at(-1), "GET /v1/clusters ")
  } finally {
    close()
  }
})

test("getCluster and listClusterNodes escape the name into the path", async () => {
  const { seen, client, close } = await clusterServer(
    async (req, res, json) => {
      if (req.url === "/v1/clusters/probe%2F1") return json(200, CLUSTER)
      if (req.url === "/v1/clusters/probe%201") return json(200, CLUSTER)
      if (req.url === "/v1/clusters/probe%201/nodes")
        return json(200, { nodes: [NODE] })
      json(404, { code: "not_found", message: "no such route" })
    },
  )
  try {
    // A slash in the name must not become a path separator.
    const c = await getCluster(client, "probe/1")
    assert.equal(c.name, "probe")
    assert.equal(seen.at(-1), "GET /v1/clusters/probe%2F1 ")

    // A space is percent-encoded, not sent raw.
    await getCluster(client, "probe 1")
    assert.equal(seen.at(-1), "GET /v1/clusters/probe%201 ")

    const nodes = await listClusterNodes(client, "probe 1")
    assert.equal(seen.at(-1), "GET /v1/clusters/probe%201/nodes ")
    assert.deepEqual(nodes, [NODE])
    assert.equal(nodes[0].sandboxes, 3)
  } finally {
    close()
  }
})

test("a node with no sandboxes reported leaves sandboxes undefined", async () => {
  const { client, close } = await clusterServer(async (req, res, json) =>
    json(200, {
      nodes: [
        { id: "i-0def", instance_type: "t4g.large", status: "provisioning" },
      ],
    }),
  )
  try {
    const [n] = await listClusterNodes(client, "probe")
    assert.deepEqual([n.id, n.status], ["i-0def", "provisioning"])
    assert.equal(n.sandboxes, undefined)
    assert.equal(n.detail, undefined)
  } finally {
    close()
  }
})

test("a cluster error carries the code, hint and status", async () => {
  const { client, close } = await clusterServer(async (req, res, json) =>
    json(403, {
      code: "forbidden",
      message: "administrator session required",
      hint: "sign in at the dashboard",
    }),
  )
  try {
    await assert.rejects(listClusters(client), (e) => {
      assert.ok(e instanceof DawnbxError)
      assert.equal(e.code, "forbidden")
      assert.equal(e.status, 403)
      assert.match(e.message, /administrator session required/)
      // The hint is folded into the message, as it is for every other failure.
      assert.match(e.message, /sign in at the dashboard/)
      return true
    })
    await assert.rejects(
      getCluster(client, "probe"),
      (e) =>
        e instanceof DawnbxError && e.code === "forbidden" && e.status === 403,
    )
    await assert.rejects(
      listClusterNodes(client, "probe"),
      (e) => e instanceof DawnbxError && e.code === "forbidden",
    )
  } finally {
    close()
  }
})

test("an error response that is not JSON becomes http_<status>", async () => {
  const { client, close } = await clusterServer(async (req, res) => {
    res.writeHead(502, "Bad Gateway", { "Content-Type": "text/html" })
    res.end("<html>bad gateway</html>")
  })
  try {
    await assert.rejects(listClusters(client), (e) => {
      assert.ok(e instanceof DawnbxError)
      // No envelope to read, so the status stands in for the code.
      assert.equal(e.code, "http_502")
      assert.equal(e.message, "Bad Gateway")
      assert.equal(e.status, 502)
      return true
    })
  } finally {
    close()
  }
})

test("a 200 whose body is not JSON rejects with a parse error", async () => {
  const { client, close } = await clusterServer(async (req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" })
    res.end("<html>not json</html>")
  })
  try {
    // The body is undecodable, so this surfaces the decoder's own error rather
    // than a DawnbxError: there is no envelope and no status to report.
    await assert.rejects(getCluster(client, "probe"), (e) => {
      assert.ok(e instanceof SyntaxError)
      assert.equal(e instanceof DawnbxError, false)
      return true
    })
  } finally {
    close()
  }
})

test("a blank cluster name is sent, not rejected locally", async () => {
  const { seen, client, close } = await clusterServer(
    async (req, res, json) => {
      if (req.url === "/v1/clusters/" || req.url === "/v1/clusters/%20%20")
        return json(400, {
          code: "invalid_request",
          message: "cluster name required",
        })
      if (req.url === "/v1/clusters//nodes")
        return json(400, {
          code: "invalid_request",
          message: "cluster name required",
        })
      json(404, { code: "not_found", message: "no such route" })
    },
  )
  try {
    // The SDK does not pre-validate the name: a blank one goes out and the
    // server's typed error is what comes back.
    await assert.rejects(getCluster(client, "  "), (e) => {
      assert.ok(e instanceof DawnbxError)
      assert.equal(e.code, "invalid_request")
      assert.equal(e.status, 400)
      return true
    })
    assert.equal(seen.at(-1), "GET /v1/clusters/%20%20 ")

    // An empty name collapses the segment; the route, not the SDK, rejects it.
    await assert.rejects(
      getCluster(client, ""),
      (e) => e instanceof DawnbxError && e.code === "invalid_request",
    )
    assert.equal(seen.at(-1), "GET /v1/clusters/ ")
    await assert.rejects(
      listClusterNodes(client, ""),
      (e) => e instanceof DawnbxError && e.code === "invalid_request",
    )
    assert.equal(seen.at(-1), "GET /v1/clusters//nodes ")
  } finally {
    close()
  }
})

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

test("cluster reads retry through cluster_unavailable", async () => {
  let tries = 0
  const { seen, client, close } = await clusterServer(
    async (req, res, json) => {
      if (++tries < 3)
        return json(503, {
          code: "cluster_unavailable",
          message: "control plane restarting",
        })
      json(200, { clusters: [CLUSTER] })
    },
  )
  try {
    const all = await listClusters(client)
    assert.equal(all.length, 1)
    assert.equal(all[0].name, "probe")
    assert.equal(tries, 3)
    assert.deepEqual(
      seen.filter((s) => s.startsWith("GET /v1/clusters")),
      ["GET /v1/clusters ", "GET /v1/clusters ", "GET /v1/clusters "],
    )
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
