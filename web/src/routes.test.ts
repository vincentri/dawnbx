// The app's router (main.tsx) and the harness router (test-support.tsx)
// declare their routes separately, because the harness swaps in an in-memory
// history. Nothing in the build keeps the two lists equal, so a route added
// to one renders in the app and cannot be reached by renderApp — the failure
// mode is a test that quietly cannot test the page. This reads both files as
// text (never as modules: importing main.tsx would mount the app) and
// compares the paths they declare.
import { describe, expect, it } from "vitest";
import app from "./main.tsx?raw";
import harness from "./test-support.tsx?raw";

// \b so basepath: "/ui" is not read as a route.
const declared = (src: string) => [...src.matchAll(/\bpath: "([^"]+)"/g)].map((m) => m[1]);

describe("route trees", () => {
  it("the app and the test harness declare the same paths", () => {
    expect(declared(harness).sort()).toEqual(declared(app).sort());
  });

  it("declares the cluster wizard, so it is reachable from the app and from tests", () => {
    expect(declared(app)).toContain("/clusters");
  });
});
