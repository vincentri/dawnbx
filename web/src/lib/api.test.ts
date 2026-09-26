// The 401 sign-out hook (api.use onResponse) is the one uncovered line here:
// openapi-fetch builds a Request from the relative /v1 URL, which only
// resolves same-origin in a browser. It is covered by using the dashboard.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { age, left, must, q, setSignedOut, when } from "./api";

const ok = (body: unknown) =>
  new Response(JSON.stringify(body), {
    status: 200,
    headers: { "content-type": "application/json" },
  });

const err = (status: number, _body?: unknown, text = "") =>
  new Response(text, { status, statusText: text, headers: { "content-type": "application/json" } });

describe("must", () => {
  it("returns data on a successful response", async () => {
    const data = { id: "sb-1" };
    await expect(must(Promise.resolve({ data, response: ok(data) }))).resolves.toEqual(data);
  });

  it("throws a sign-in message on 401, whatever the body says", async () => {
    const p = Promise.resolve({
      error: { message: "unauthorized" },
      response: new Response(null, { status: 401 }),
    });
    await expect(must(p)).rejects.toThrow(/sign in again/);
  });

  it("throws the server message with its hint", async () => {
    const response = err(400);
    const p = Promise.resolve({
      error: { message: "ttl invalid", hint: "use 30m or 1h" },
      response,
    });
    await expect(must(p)).rejects.toThrow("ttl invalid (use 30m or 1h)");
  });

  it("throws the bare message when the server sends no hint", async () => {
    const p = Promise.resolve({ error: { message: "disk_low" }, response: err(507) });
    await expect(must(p)).rejects.toThrow("disk_low");
  });

  it("falls back to status and statusText when there is no error body", async () => {
    const p = Promise.resolve({
      response: new Response(null, { status: 502, statusText: "Bad Gateway" }),
    });
    await expect(must(p)).rejects.toThrow("502 Bad Gateway");
  });
});

describe("q", () => {
  it("single-quotes for sh -c", () => {
    expect(q("ls -1")).toBe("'ls -1'");
  });

  it("escapes an embedded single quote", () => {
    expect(q("echo 'hi'")).toBe(`'echo '\\''hi'\\'''`);
  });
});

describe("age, left, when", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-02T00:00:00Z"));
  });
  afterEach(() => vi.useRealTimers());

  it("formats age in seconds, minutes, hours and days", () => {
    expect(age("2026-01-01T23:59:30Z")).toBe("30s");
    expect(age("2026-01-01T23:30:00Z")).toBe("30m");
    expect(age("2026-01-01T12:00:00Z")).toBe("12h 0m");
    expect(age("2025-12-31T00:00:00Z")).toBe("2d");
  });

  it("formats the time left, and says never for a keep-forever sandbox", () => {
    expect(left("2026-01-02T01:00:00Z")).toBe("1h 0m");
    expect(left(null)).toBe("never");
    expect(left("2026-01-02T00:30:00Z")).toBe("30m");
  });

  it("renders an expiry, or a dash when there is none", () => {
    expect(when("2026-01-02T03:00:00Z")).toBe(new Date("2026-01-02T03:00:00Z").toLocaleString());
    expect(when(null)).toBe("–");
  });
});

describe("setSignedOut", () => {
  it("registers the callback the 401 hook calls", () => {
    let called = 0;
    setSignedOut(() => called++);
    expect(typeof setSignedOut(() => {})).toBe("function");
    expect(called).toBe(0);
  });
});
