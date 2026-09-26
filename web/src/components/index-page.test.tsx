// @vitest-environment jsdom
import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { countTo, installFetch, type Routes, releaseFetch } from "@/test-fetch";
import { ADMIN, renderApp } from "@/test-support";

afterEach(releaseFetch);

// The index is the sandboxes list on a cluster, because that is where an agent
// starts. On a control plane those routes are a hard 503, so rendering it there
// put an error above a live create form — the target deployment mode opened on an
// error page. The nav already hid the link, which is exactly why it went
// unnoticed: the link was never the problem, the landing route was.
describe("The landing page", () => {
  const at = (controlPlane: boolean, extra: Routes = {}) => {
    const calls = installFetch({
      "GET /v1/me": { json: ADMIN },
      "GET /v1/control-plane": {
        json: { control_plane: controlPlane, providers: ["aws"], version: "v1" },
      },
      "GET /v1/clusters": { json: { clusters: [] } },
      "GET /v1/providers": { json: { providers: [{ id: "aws", available: true, delivery: "x" }] } },
      ...extra,
    });
    renderApp({ entry: "/", me: ADMIN });
    return calls;
  };

  it("shows the sandboxes list on a cluster, which is where an agent starts", async () => {
    at(false);
    // Sandboxes opens with a create form; clusters has no image input.
    expect(await screen.findByLabelText("image")).toBeInTheDocument();
  });

  it("sends a control plane to the cluster list instead", async () => {
    at(true);
    // The cluster list is up, and the sandboxes create form never rendered.
    expect(await screen.findByText(/cluster/i)).toBeInTheDocument();
    expect(screen.queryByLabelText("image")).not.toBeInTheDocument();
  });

  it("does not ask for sandboxes at all on a control plane", async () => {
    const calls = at(true);
    await screen.findByText(/cluster/i);
    // One fewer round trip, and no error page in front of the operator.
    expect(countTo(calls, "GET", "/v1/sandboxes")).toBe(0);
  });
});
