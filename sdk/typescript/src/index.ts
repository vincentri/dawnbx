// dawnbx TypeScript SDK. Zero dependencies; needs Node 18+ (global fetch).
//
//   await using sb = await Sandbox.create();
//   const r = await sb.exec("python -c 'print(1+1)'");

export interface ClientOptions {
  /** Server URL. Default: $DAWNBX_URL, else http://127.0.0.1:8080. */
  url?: string
  /** API key. Default: $DAWNBX_API_KEY. */
  apiKey?: string
}

export interface CreateOptions extends ClientOptions {
  /** Default python:3.12-slim. ENTRYPOINT/CMD is not run. */
  image?: string
  /** Duration like "30m" or "2h"; null keeps it until killed. Default "1h". */
  ttl?: string | null
  /** Default "internet". "none" blocks all network access. */
  network?: "internet" | "none"
  cpu?: string
  memory?: string
}

export interface ExecOptions {
  /** Seconds; null = no limit. Default 600. */
  timeout?: number | null
  /** Start and return immediately; output goes to `log` inside the sandbox. */
  background?: boolean
  env?: Record<string, string>
}

export interface ExecResult {
  exitCode: number
  stdout: string
  stderr: string
  /** Set for background execs. */
  pid?: number
  log?: string
}

export interface SandboxInfo {
  id: string
  image: string
  status: "running" | "stopped"
  reason?: string
  parent?: string
  network: string
  created: Date
  expiresAt: Date | null
  /** Set when the sandbox was recreated (node reboot); processes did not survive, files did. */
  restartedAt: Date | null
  warnings?: string[]
}

/** Every API failure. `code` is stable; `hint` says what to do. */
export class DawnbxError extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly hint?: string,
    readonly status?: number,
  ) {
    super(hint ? `${message} (${hint})` : message)
    this.name = "DawnbxError"
  }
}

/**
 * The HTTP client. Exported so the control-plane helpers below have something a
 * caller can actually pass: they take a `Client`, so keeping this private would
 * make them uncallable from outside the module.
 */
/**
 * A handle on one dawnbx server: its URL and the key it authenticates with.
 *
 * Exported because the cluster-management helpers below take one, and a public
 * function whose parameter type is not exported is an API nobody outside this
 * package can call. Sandbox remains the way to *use* a cluster; a Client is how
 * you address a server, which for cluster management is a control plane rather
 * than a cluster.
 */
export class Client {
  readonly url: string
  private readonly key: string

  constructor(o: ClientOptions = {}) {
    const env: Record<string, string | undefined> =
      (globalThis as any).process?.env ?? {}
    this.url = (o.url ?? env.DAWNBX_URL ?? "http://127.0.0.1:8080").replace(
      /\/+$/,
      "",
    )
    const key = o.apiKey ?? env.DAWNBX_API_KEY
    if (!key) {
      throw new DawnbxError(
        "unauthorized",
        "no API key",
        "set DAWNBX_API_KEY to the key the installer printed",
      )
    }
    this.key = key
  }

  async req(
    method: string,
    path: string,
    body?: unknown,
    raw = false,
  ): Promise<any> {
    const init: RequestInit = {
      method,
      headers: { Authorization: `Bearer ${this.key}` },
    }
    if (body instanceof Uint8Array || typeof body === "string") {
      init.body = body as BodyInit
    } else if (body !== undefined) {
      init.body = JSON.stringify(body)
      ;(init.headers as Record<string, string>)["Content-Type"] =
        "application/json"
    }
    // Reads are safe to repeat while the node restarts (cluster_unavailable).
    const tries = method === "GET" ? 3 : 1
    for (let i = 1; ; i++) {
      let res: Response
      try {
        res = await fetch(this.url + path, init)
      } catch (e) {
        if (i < tries) {
          await sleep(1000 * i)
          continue
        }
        throw new DawnbxError(
          "connection_failed",
          `cannot reach ${this.url}: ${(e as Error).message}`,
          "check DAWNBX_URL and that the server is running (systemctl status dawnbx)",
        )
      }
      if (res.ok) {
        if (res.status === 204) return undefined
        return raw ? new Uint8Array(await res.arrayBuffer()) : res.json()
      }
      const err = await res
        .json()
        .catch(() => ({ code: "http_" + res.status, message: res.statusText }))
      if (err.code === "cluster_unavailable" && i < tries) {
        await sleep(1000 * i)
        continue
      }
      throw new DawnbxError(err.code, err.message, err.hint, res.status)
    }
  }
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
const date = (s: string | null | undefined) => (s ? new Date(s) : null)

function info(v: any): SandboxInfo {
  return {
    id: v.id,
    image: v.image,
    status: v.status,
    reason: v.reason,
    parent: v.parent,
    network: v.network,
    created: new Date(v.created),
    expiresAt: date(v.expires_at),
    restartedAt: date(v.restarted_at),
    warnings: v.warnings,
  }
}

// Symbol.asyncDispose ships in Node 22+; this lets `await using` work on 18/20 when transpiled.
;(Symbol as any).asyncDispose ??= Symbol.for("Symbol.asyncDispose")

export class Sandbox {
  readonly files: Files

  private constructor(
    private readonly c: Client,
    public info: SandboxInfo,
  ) {
    this.files = new Files(c, info.id)
  }

  get id() {
    return this.info.id
  }

  /** Create a sandbox and wait until it can run commands. */
  static async create(o: CreateOptions = {}): Promise<Sandbox> {
    const c = new Client(o)
    const body: Record<string, unknown> = {}
    for (const k of ["image", "network", "cpu", "memory"] as const)
      if (o[k] !== undefined) body[k] = o[k]
    if (o.ttl !== undefined) body.ttl = o.ttl // null is sent: keep until killed
    return new Sandbox(c, info(await c.req("POST", "/v1/sandboxes", body)))
  }

  static async get(id: string, o: ClientOptions = {}): Promise<Sandbox> {
    const c = new Client(o)
    return new Sandbox(
      c,
      info(await c.req("GET", `/v1/sandboxes/${encodeURIComponent(id)}`)),
    )
  }

  static async list(o: ClientOptions = {}): Promise<SandboxInfo[]> {
    const r = await new Client(o).req("GET", "/v1/sandboxes")
    return r.sandboxes.map(info)
  }

  /** Run a shell command. A nonzero exit code is returned, not thrown. */
  async exec(cmd: string, o: ExecOptions = {}): Promise<ExecResult> {
    const body: Record<string, unknown> = { cmd }
    if (o.timeout !== undefined) body.timeout = o.timeout
    if (o.background) body.background = true
    if (o.env) body.env = o.env
    const r = await this.c.req("POST", `/v1/sandboxes/${this.id}/exec`, body)
    return {
      exitCode: r.exit_code,
      stdout: r.stdout,
      stderr: r.stderr,
      pid: r.pid,
      log: r.log,
    }
  }

  /**
   * Copy this sandbox's /workspace into `count` new sandboxes (default 1, max 10).
   * Running processes are paused during the copy; children start with fresh processes.
   * `ttl` works like create(): default "1h", null keeps them until killed.
   */
  async fork(count = 1, o: { ttl?: string | null } = {}): Promise<Sandbox[]> {
    const body: Record<string, unknown> = { count }
    if (o.ttl !== undefined) body.ttl = o.ttl
    const r = await this.c.req("POST", `/v1/sandboxes/${this.id}/fork`, body)
    return r.sandboxes.map((v: any) => new Sandbox(this.c, info(v)))
  }

  /** New TTL from now ("2h"), or null to keep until killed. */
  async extend(ttl: string | null): Promise<void> {
    this.info = info(
      await this.c.req("POST", `/v1/sandboxes/${this.id}/extend`, { ttl }),
    )
  }

  /** Restart a stopped sandbox (files kept). */
  async start(): Promise<void> {
    this.info = info(await this.c.req("POST", `/v1/sandboxes/${this.id}/start`))
  }

  async refresh(): Promise<SandboxInfo> {
    this.info = info(await this.c.req("GET", `/v1/sandboxes/${this.id}`))
    return this.info
  }

  /** Delete the sandbox and its files. */
  async kill(): Promise<void> {
    try {
      await this.c.req("DELETE", `/v1/sandboxes/${this.id}`)
    } catch (e) {
      if (!(e instanceof DawnbxError && e.code === "not_found")) throw e
    }
  }

  async [Symbol.asyncDispose]() {
    await this.kill()
  }
}

class Files {
  constructor(
    private readonly c: Client,
    private readonly id: string,
  ) {}

  private path(p: string) {
    return `/v1/sandboxes/${this.id}/files?path=${encodeURIComponent(p)}`
  }

  /** Read a file as UTF-8. Relative paths are under /workspace. */
  async read(path: string): Promise<string> {
    return new TextDecoder().decode(await this.readBytes(path))
  }

  async readBytes(path: string): Promise<Uint8Array> {
    return this.c.req("GET", this.path(path), undefined, true)
  }

  /** Write a file, creating parent directories. */
  async write(path: string, data: string | Uint8Array): Promise<void> {
    await this.c.req("PUT", this.path(path), data)
  }
}
