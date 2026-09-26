// The app's router (main.tsx) and the harness router (test-support.tsx)
// declare their routes separately, because the harness swaps in an in-memory
// history. Nothing in the build keeps the two lists equal, so a route added
// to one renders in the app and cannot be reached by renderApp — the failure
// mode is a test that quietly cannot test the page. This reads both files as
// text (never as modules: importing main.tsx would mount the app) and compares
// what they declare.
//
// Comparing paths alone is not enough. A route present in both trees but
// pointing at a different component, or missing its search validator, is a test
// that reaches a page the app never shows. So this compares the path *and* the
// component *and* the validator by name, which is what the two files can
// actually disagree about.
import { describe, expect, it } from "vitest";
import app from "./main.tsx?raw";
import harness from "./test-support.tsx?raw";

// Anchored on createRoute so this reads route declarations and nothing else,
// and terminated on the closing "})" so a route entry cannot swallow the next
// one. The block between is everything the two trees can disagree about.
const ROUTE = /createRoute\(\{([\s\S]*?)\}\)/g;
const field = (body: string, name: string) =>
  body.match(new RegExp(`\\b${name}:\\s*"?([^",\\n]+)`))?.[1];

type Route = { path: string; component: string; hasSearch: boolean };

const declared = (src: string): Route[] =>
  [...src.matchAll(ROUTE)]
    .map((m) => ({ body: m[1], path: field(m[1], "path") }))
    .filter((r): r is { body: string; path: string } => r.path !== undefined)
    .map((r) => ({
      path: r.path.trim(),
      component: (field(r.body, "component") ?? "").trim(),
      // A validateSearch is what makes useSearch({from}) typed for that route,
      // so its absence in one tree and not the other is a real divergence.
      hasSearch: r.body.includes("validateSearch"),
    }));

describe("route trees", () => {
  it("the app and the test harness declare the same paths", () => {
    expect(
      declared(harness)
        .map((r) => r.path)
        .sort(),
    ).toEqual(
      declared(app)
        .map((r) => r.path)
        .sort(),
    );
  });

  it("every route points at the same component in both trees", () => {
    const byPath = (src: string) => new Map(declared(src).map((r) => [r.path, r]));
    const a = byPath(app);
    const h = byPath(harness);
    for (const [path, route] of a) {
      expect(h.get(path)?.component, `${path} is a different component in the harness`).toBe(
        route.component,
      );
    }
  });

  it("every route keeps its search validator in both trees", () => {
    const byPath = (src: string) => new Map(declared(src).map((r) => [r.path, r]));
    const a = byPath(app);
    const h = byPath(harness);
    for (const [path, route] of a) {
      expect(h.get(path)?.hasSearch, `${path} lost its validateSearch in the harness`).toBe(
        route.hasSearch,
      );
    }
  });

  it("declares the cluster wizard, so it is reachable from the app and from tests", () => {
    expect(declared(app).map((r) => r.path)).toContain("/clusters");
  });

  it("actually compared something, so an empty match cannot pass", () => {
    // A regex that stopped matching would make every assertion above trivially
    // true, which is the same class of quiet failure this file exists to catch.
    expect(declared(app).length).toBeGreaterThan(2);
  });
});
