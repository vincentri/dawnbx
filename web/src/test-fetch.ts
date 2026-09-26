// The one network boundary. `openapi-fetch` captures `globalThis.fetch` when
// `@/lib/api` is first imported, so the stub has to be in place before any
// test module loads — test-setup.ts imports this file for that reason.
import { vi } from "vitest";

export type FetchCall = { method: string; url: string; body: unknown };

export type Reply = {
  status?: number;
  json?: unknown;
  text?: string;
  contentType?: string;
};

export type Route = (call: FetchCall) => Reply | Promise<Reply>;

export type Routes = Record<string, Route | Reply>;

const realFetch = globalThis.fetch.bind(globalThis);

let table: Record<string, Route | Reply> | null = null;
let log: FetchCall[] = [];

async function stub(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  // With no routes installed (e.g. a node-env unit test), behave like plain
  // fetch so a test that stubbed globalThis.fetch itself still wins.
  if (!table) {
    const live = globalThis.fetch;
    return live === stub ? realFetch(input, init) : live(input, init);
  }
  const req = input instanceof Request ? input : new Request(input, init);
  const url = new URL(req.url);
  const raw = await req.clone().text();
  let body: unknown = raw;
  if (raw && (req.headers.get("content-type") ?? "").includes("json")) {
    try {
      body = JSON.parse(raw);
    } catch {
      body = raw;
    }
  }
  const call: FetchCall = {
    method: req.method,
    url: url.pathname + url.search,
    body,
  };
  log.push(call);
  const key = `${req.method} ${url.pathname}`;
  const r = table[key];
  if (!r)
    throw new Error(
      `unmocked request: ${key} (mocked: ${Object.keys(table).join(", ") || "none"})`,
    );
  const reply = typeof r === "function" ? await r(call) : r;
  const status = reply.status ?? 200;
  const payload =
    reply.text !== undefined
      ? reply.text
      : status === 204
        ? null
        : JSON.stringify(reply.json ?? null);
  const res = new Response(payload, {
    status,
    headers: { "content-type": reply.contentType ?? "application/json" },
  });
  // `must()` branches on response.url; a constructed Response has none.
  Object.defineProperty(res, "url", { value: req.url });
  return res;
}

globalThis.fetch = stub as unknown as typeof fetch;

/** Routes every request the app makes, and hands back the call log. */
export function installFetch(routes: Record<string, Route | Reply>): FetchCall[] {
  table = routes;
  log = [];
  vi.stubGlobal("fetch", stub);
  return log;
}

export function releaseFetch() {
  table = null;
  log = [];
  vi.unstubAllGlobals();
}

// writeErr marshals sandbox.Error flat, matching components.schemas.Error.
const errBody = (message: string, hint?: string) => ({
  code: "error",
  message,
  hint,
});

export function apiError(message: string, hint?: string, status = 400): Reply {
  return { status, json: errBody(message, hint) };
}

/** What the server returns when the session cookie is gone. */
export const noSession: Reply = apiError("no session", "sign in again", 401);

/** An error with a specific envelope code, for the pages that branch on one. */
export const codedError = (
  code: string,
  message: string,
  status: number,
  hint?: string,
): Reply => ({
  status,
  json: { code, message, hint },
});

/** Body of the single call to `path` (exact pathname), or throws. */
export function bodyOf(calls: FetchCall[], method: string, path: string): unknown {
  const hits = calls.filter(
    (c) => c.method === method && new URL(c.url, "http://x").pathname === path,
  );
  if (hits.length !== 1)
    throw new Error(`expected exactly one ${method} ${path}, saw ${hits.length}`);
  return hits[0].body;
}

export const countTo = (calls: FetchCall[], method: string, path: string): number =>
  calls.filter((c) => c.method === method && new URL(c.url, "http://x").pathname === path).length;
