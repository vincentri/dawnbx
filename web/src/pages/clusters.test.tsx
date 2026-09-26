// @vitest-environment jsdom
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  apiError,
  codedError,
  countTo,
  type FetchCall,
  installFetch,
  type Reply,
  type Routes,
  releaseFetch,
} from "@/test-fetch";
import { ADMIN, ME, renderApp } from "@/test-support";

afterEach(releaseFetch);

const CLUSTER_PATH = "/v1/clusters/boxy";
const NODES_PATH = `${CLUSTER_PATH}/nodes`;

const cluster = (over: Record<string, unknown> = {}) => ({
  name: "boxy",
  provider: "aws",
  region: "eu-west-1",
  instance_type: "t4g.medium",
  disk_gib: 30,
  domain: "",
  status: "ready",
  hourly_usd: 0.02,
  monthly_usd: 14.6,
  created: "2026-09-26T10:00:00Z",
  updated: "2026-09-26T10:05:00Z",
  ...over,
});

const worker = (over: Record<string, unknown> = {}) => ({
  id: "w-1",
  instance_type: "t4g.medium",
  status: "ready",
  sandboxes: 0,
  created: "2026-09-26T10:06:00Z",
  ...over,
});

const QUOTE = {
  quote_id: "q-1",
  hourly_usd: 0.02,
  monthly_usd: 14.6,
  lines: [
    { label: "compute", hourly_usd: 0.0168, monthly_usd: 12.26 },
    { label: "storage", hourly_usd: 0.0012, monthly_usd: 0.88 },
    { label: "public_ipv4", hourly_usd: 0.002, monthly_usd: 1.46 },
  ],
  excluded: ["data_transfer", "taxes", "provider_discounts"],
};

const PROVIDERS = {
  json: {
    providers: [
      { id: "aws", available: true },
      { id: "gcp", available: false },
      { id: "azure", available: false },
    ],
  },
};
const CATALOGUE = {
  json: {
    instance_types: [
      { id: "t4g.medium", hourly_usd: 0.0168, monthly_usd: 12.26, region: "eu-west-1" },
      { id: "m7g.large", hourly_usd: 0.0816, monthly_usd: 59.57, region: "eu-west-1" },
    ],
  },
};

/** The routes the page and the shell need; tests add the ones their click needs. */
const base = (extra: Routes = {}): Routes => ({
  "GET /v1/me": { json: ADMIN },
  "GET /v1/control-plane": { json: { control_plane: true, providers: ["aws"], version: "v1" } },
  "GET /v1/clusters": { json: { clusters: [] } },
  "GET /v1/providers": PROVIDERS,
  ...extra,
});

const posted = (calls: FetchCall[], method: string, path: string) =>
  calls.find((c) => c.method === method && new URL(c.url, "http://x").pathname === path)?.body;

/** Walks the wizard to the price review, which is the gate on spending money. */
const toPrice = async (domain?: string) => {
  await userEvent.click(await screen.findByRole("button", { name: /^aws/ }));
  await userEvent.type(await screen.findByLabelText("cluster name"), "boxy");
  if (domain) await userEvent.type(await screen.findByLabelText("domain"), domain);
  await userEvent.click(screen.getByLabelText("region"));
  await userEvent.click(await screen.findByRole("option", { name: "eu-west-1" }));
  await userEvent.click(screen.getByLabelText("size"));
  await userEvent.click(await screen.findByRole("option", { name: /^t4g\.medium/ }));
  await userEvent.click(screen.getByRole("button", { name: "Estimate price" }));
  return screen.findByRole("button", { name: "Confirm and create" });
};

/** Confirm-and-create through the dialog, which is where the money is spent. */
const confirmCreate = async () => {
  await userEvent.click(await screen.findByRole("button", { name: "Confirm and create" }));
  const dialog = await screen.findByRole("dialog");
  await userEvent.click(within(dialog).getByRole("button", { name: "Create cluster" }));
};

const clickIn = async (name: string) => {
  await userEvent.click(await screen.findByRole("button", { name }));
  const dialog = await screen.findByRole("dialog");
  return dialog;
};

// A failed action is also toasted, so the assertion has to look inside the
// page to prove the message is shown where the operator is looking.
const inPage = async (text: string | RegExp) =>
  within(await screen.findByRole("main")).findByText(text);

describe("Provider choice", () => {
  it("lists every provider the server reports and refuses the ones it cannot provision", async () => {
    const calls = installFetch(base());
    renderApp({ entry: "/clusters", me: ADMIN });

    expect(await screen.findByRole("button", { name: /^aws/ })).toBeEnabled();
    const gcp = screen.getByRole("button", { name: /^gcp/ });
    const azure = screen.getByRole("button", { name: /^azure/ });
    expect(gcp).toBeDisabled();
    expect(gcp).toHaveTextContent("unavailable");
    expect(azure).toBeDisabled();
    // Nothing to click, so no provider-specific request is ever made (SC-006).
    expect(countTo(calls, "GET", "/v1/providers/gcp/regions")).toBe(0);
    expect(screen.getByRole("heading", { name: "Provider" })).toBeInTheDocument();
  });

  it("says so when the provider list cannot be read", async () => {
    installFetch(
      base({ "GET /v1/providers": apiError("store offline", "retry in a minute", 503) }),
    );
    renderApp({ entry: "/clusters", me: ADMIN });

    expect(await screen.findByText("store offline (retry in a minute)")).toBeInTheDocument();
  });

  it("invites the operator to start when the server reports no provider", async () => {
    installFetch(base({ "GET /v1/providers": { json: { providers: [] } } }));
    renderApp({ entry: "/clusters", me: ADMIN });

    expect(await screen.findByText("The server reports no provider.")).toBeInTheDocument();
  });

  it("is for control-plane administrators", async () => {
    installFetch(base());
    renderApp({ entry: "/clusters", me: ME });

    expect(
      await screen.findByText("Clusters are managed by control-plane administrators."),
    ).toBeInTheDocument();
    expect(countTo(installFetch(base()), "GET", "/v1/providers")).toBe(0);
  });
});

describe("Configuration", () => {
  const catalogue = {
    "GET /v1/providers/aws/regions": { json: { regions: ["eu-west-1", "us-east-1"] } },
    "GET /v1/providers/aws/instance-types": CATALOGUE,
  };

  it("prices the chosen sizes and says which region the catalogue is for", async () => {
    installFetch(
      base({
        ...catalogue,
        "GET /v1/providers/aws/instance-types": {
          json: {
            instance_types: [
              { id: "t4g.medium", hourly_usd: 0.0168, monthly_usd: 12.26, region: "us-east-1" },
            ],
          },
        },
      }),
    );
    renderApp({ entry: "/clusters?step=configure&provider=aws", me: ADMIN });

    await userEvent.click(await screen.findByLabelText("size"));
    const option = await screen.findByRole("option", { name: /^t4g\.medium/ });
    expect(option).toHaveTextContent("$0.0168/h · $12.26/mo");
    expect(
      await screen.findByText(/These prices are the provider's catalogue for us-east-1, not /),
    ).toBeInTheDocument();
  });

  it("will not estimate until the cluster is named and a region and size are chosen", async () => {
    const calls = installFetch(base(catalogue));
    renderApp({ entry: "/clusters?step=configure&provider=aws", me: ADMIN });

    const estimate = await screen.findByRole("button", { name: "Estimate price" });
    expect(estimate).toBeDisabled();
    expect(
      await screen.findByText("Name the cluster and pick a region and a size to see a price."),
    ).toBeInTheDocument();
    expect(countTo(calls, "POST", "/v1/providers/aws/estimate")).toBe(0);
  });

  it("keeps the operator's choices and reports the constraint when a region cannot be listed", async () => {
    const calls = installFetch(
      base({
        ...catalogue,
        "GET /v1/providers/aws/regions": apiError(
          "provider unavailable",
          "this control plane has no credential source for aws",
          400,
        ),
      }),
    );
    renderApp({ entry: "/clusters?step=configure&provider=aws", me: ADMIN });

    await userEvent.type(await screen.findByLabelText("cluster name"), "boxy");
    expect(
      await screen.findByText(
        "provider unavailable (this control plane has no credential source for aws)",
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("cluster name")).toHaveValue("boxy");
    expect(countTo(calls, "POST", "/v1/providers/aws/estimate")).toBe(0);
  });

  it("shows the server's reason when the estimate cannot be made", async () => {
    installFetch(
      base({
        ...catalogue,
        "POST /v1/providers/aws/estimate": apiError(
          "instance type not offered in eu-west-1",
          "pick another size",
          400,
        ),
      }),
    );
    renderApp({ entry: "/clusters?step=configure&provider=aws", me: ADMIN });

    await userEvent.type(await screen.findByLabelText("cluster name"), "boxy");
    await userEvent.click(screen.getByLabelText("region"));
    await userEvent.click(await screen.findByRole("option", { name: "eu-west-1" }));
    await userEvent.click(screen.getByLabelText("size"));
    await userEvent.click(await screen.findByRole("option", { name: /^t4g\.medium/ }));
    await userEvent.click(screen.getByRole("button", { name: "Estimate price" }));

    expect(
      await inPage("instance type not offered in eu-west-1 (pick another size)"),
    ).toBeInTheDocument();
  });
});

describe("Price review", () => {
  const priced = (extra: Routes = {}) =>
    base({
      "GET /v1/providers/aws/regions": { json: { regions: ["eu-west-1"] } },
      "GET /v1/providers/aws/instance-types": CATALOGUE,
      "POST /v1/providers/aws/estimate": { json: QUOTE },
      ...extra,
    });

  it("shows the fixed-charge breakdown and what is not estimated before the confirm button", async () => {
    const calls = installFetch(priced());
    renderApp({ entry: "/clusters", me: ADMIN });

    await toPrice();

    const table = await screen.findByRole("table");
    for (const label of ["compute", "storage", "public_ipv4"]) {
      expect(within(table).getByText(label)).toBeInTheDocument();
    }
    expect(within(table).getByText("$0.0200")).toBeInTheDocument();
    expect(within(table).getByText("$14.60")).toBeInTheDocument();
    expect(
      screen.getByText(/Not estimated: data_transfer, taxes, provider_discounts/),
    ).toBeInTheDocument();
    expect(screen.getByText(/Priced for t4g\.medium · eu-west-1 · 30 GiB/)).toBeInTheDocument();
    // The breakdown is on screen; nothing has been spent.
    expect(countTo(calls, "POST", "/v1/clusters")).toBe(0);
  });

  it("creates nothing when the price step is reached without an estimate", async () => {
    const calls = installFetch(priced());
    renderApp({ entry: "/clusters?step=price", me: ADMIN });

    expect(
      await screen.findByText("No estimate yet, so there is nothing to confirm."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Confirm and create" })).not.toBeInTheDocument();
    expect(countTo(calls, "POST", "/v1/clusters")).toBe(0);
  });

  it("creates the cluster with the reviewed quote only after the confirmation", async () => {
    const calls = installFetch(
      priced({
        "POST /v1/clusters": { json: cluster({ status: "provisioning", phase: "launching" }) },
        [`GET ${CLUSTER_PATH}`]: { json: cluster({ status: "provisioning", phase: "launching" }) },
      }),
    );
    renderApp({ entry: "/clusters", me: ADMIN });

    await toPrice("boxes.example");
    await confirmCreate();

    await waitFor(() => expect(countTo(calls, "POST", "/v1/clusters")).toBe(1));
    expect(posted(calls, "POST", "/v1/clusters")).toEqual({
      name: "boxy",
      region: "eu-west-1",
      instance_type: "t4g.medium",
      disk_gib: 30,
      domain: "boxes.example",
      quote_id: "q-1",
    });
    // Creation hands the operator to the cluster's own status page.
    expect(await screen.findByText(/launching/)).toBeInTheDocument();
  });

  it("surfaces a stale quote and asks for a new review", async () => {
    installFetch(
      priced({
        "POST /v1/clusters": codedError(
          "quote_stale",
          "the estimate no longer matches the configuration",
          409,
          "estimate again",
        ),
      }),
    );
    renderApp({ entry: "/clusters", me: ADMIN });

    await toPrice();
    await confirmCreate();

    expect(await inPage(/The price changed before the cluster was created/)).toBeInTheDocument();
    expect(screen.getByText("Change configuration")).toBeInTheDocument();
  });

  it("shows the server's reason when the create is refused for another reason", async () => {
    installFetch(
      priced({ "POST /v1/clusters": apiError("invalid_request", "name is already taken", 400) }),
    );
    renderApp({ entry: "/clusters", me: ADMIN });

    await toPrice();
    await confirmCreate();

    expect(await inPage("invalid_request (name is already taken)")).toBeInTheDocument();
  });
});

describe("Cluster status", () => {
  const at = (entry: string, extra: Routes) => {
    installFetch(base({ [`GET ${CLUSTER_PATH}`]: { json: cluster() }, ...extra }));
    return renderApp({ entry, me: ADMIN });
  };

  it.each(["provisioning", "ready", "failed", "deleting", "deleted"])(
    "renders the %s phase",
    async (status) => {
      at("/clusters?step=status&name=boxy", {
        [`GET ${CLUSTER_PATH}`]: { json: cluster({ status, phase: "launching" }) },
      });
      // A terminal state is on the timeline and in the badge; both say it.
      expect(await screen.findAllByText(status)).not.toHaveLength(0);
    },
  );

  it("shows the server's own sub-step beside the stage it is in", async () => {
    at("/clusters?step=status&name=boxy", {
      [`GET ${CLUSTER_PATH}`]: { json: cluster({ status: "provisioning", phase: "verifying" }) },
    });
    expect(await screen.findByText("provisioning: verifying")).toBeInTheDocument();
  });

  it("gives a failure its reason, its next step and no secret", async () => {
    at("/clusters?step=status&name=boxy", {
      [`GET ${CLUSTER_PATH}`]: {
        json: cluster({
          status: "failed",
          phase: "verifying",
          detail: "the host never accepted the injected password",
        }),
      },
    });

    expect(await screen.findByText("Provisioning failed in verifying.")).toBeInTheDocument();
    expect(screen.getByText("the host never accepted the injected password")).toBeInTheDocument();
    expect(screen.getByText(/Next step: read the reason above/)).toBeInTheDocument();
    expect(screen.getByText(/SSH is for rescue work/)).toBeInTheDocument();
  });

  it("falls back to a plain reason when the provider gave none", async () => {
    at("/clusters?step=status&name=boxy", {
      [`GET ${CLUSTER_PATH}`]: { json: cluster({ status: "failed", detail: "" }) },
    });
    expect(await screen.findByText("The provider reported no reason.")).toBeInTheDocument();
  });

  it("shows the URL, the public IP and the certificate pin once ready", async () => {
    at("/clusters?step=status&name=boxy", {
      [`GET ${CLUSTER_PATH}`]: {
        json: cluster({
          url: "https://boxy.example",
          public_ip: "203.0.113.7",
          tls_pin: "sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
        }),
      },
    });

    expect(await screen.findByRole("link", { name: "https://boxy.example" })).toHaveAttribute(
      "href",
      "https://boxy.example",
    );
    expect(screen.getByText("Public IP 203.0.113.7")).toBeInTheDocument();
    expect(
      screen.getByText("sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
    ).toBeInTheDocument();
    expect(screen.getByText(/compare it out of band/)).toBeInTheDocument();
  });

  it("shows the server's reason when the cluster cannot be read", async () => {
    at("/clusters?step=status&name=boxy", {
      [`GET ${CLUSTER_PATH}`]: apiError("no such cluster", "pick one from the list", 404),
    });
    expect(await screen.findByText("no such cluster (pick one from the list)")).toBeInTheDocument();
  });

  it("sends the operator back to the wizard when no cluster is named", async () => {
    at("/clusters?step=status", {});
    await userEvent.click(await screen.findByRole("button", { name: "New cluster" }));
    expect(await screen.findByRole("heading", { name: "Provider" })).toBeInTheDocument();
  });

  it("polls a provisioning cluster every 5s", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = installFetch(
        base({ [`GET ${CLUSTER_PATH}`]: { json: cluster({ status: "provisioning" }) } }),
      );
      renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });
      await vi.advanceTimersByTimeAsync(0);
      expect(countTo(calls, "GET", CLUSTER_PATH)).toBe(1);

      await vi.advanceTimersByTimeAsync(5000);
      expect(countTo(calls, "GET", CLUSTER_PATH)).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops polling once the outcome is known", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = installFetch(base({ [`GET ${CLUSTER_PATH}`]: { json: cluster() } }));
      renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(20000);
      expect(countTo(calls, "GET", CLUSTER_PATH)).toBe(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("Cluster credentials", () => {
  const at = (extra: Routes) => {
    installFetch(base({ [`GET ${CLUSTER_PATH}`]: { json: cluster() }, ...extra }));
    return renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });
  };

  it("reveals the pair once the cluster has minted its key", async () => {
    at({
      [`GET ${CLUSTER_PATH}/credentials`]: {
        json: { api_key: "dbx_1_secret", admin_password: "hunter2hunter2" },
      },
    });

    await userEvent.click(await screen.findByRole("button", { name: "Reveal" }));
    expect(await screen.findByText("dbx_1_secret")).toBeInTheDocument();
    expect(screen.getByText("hunter2hunter2")).toBeInTheDocument();
  });

  it("says the key is not minted yet before the cluster is ready", async () => {
    at({
      [`GET ${CLUSTER_PATH}`]: { json: cluster({ status: "provisioning", phase: "verifying" }) },
      [`GET ${CLUSTER_PATH}/credentials`]: codedError(
        "credentials_not_ready",
        "the key is minted while the cluster verifies",
        409,
      ),
    });

    await userEvent.click(await screen.findByRole("button", { name: "Reveal" }));
    expect(await inPage(/The cluster mints its API key while it verifies/)).toBeInTheDocument();
  });

  it("shows the server's reason when the reveal is refused for another reason", async () => {
    at({
      [`GET ${CLUSTER_PATH}/credentials`]: apiError("forbidden", "admin only", 403),
    });
    await userEvent.click(await screen.findByRole("button", { name: "Reveal" }));
    expect(await inPage("forbidden (admin only)")).toBeInTheDocument();
  });

  it("rotates behind a confirmation and re-reads the pair", async () => {
    const calls = installFetch(
      base({
        [`GET ${CLUSTER_PATH}`]: { json: cluster() },
        [`GET ${CLUSTER_PATH}/credentials`]: {
          json: { api_key: "dbx_2_new", admin_password: "newpassword" },
        },
        [`POST ${CLUSTER_PATH}/rotate`]: {
          json: cluster({
            detail: "the running cluster keeps the old pair until you apply the new one",
          }),
        },
      }),
    );
    renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });

    const dialog = await clickIn("Rotate");
    await userEvent.click(within(dialog).getByRole("button", { name: "Rotate" }));

    await waitFor(() => expect(countTo(calls, "POST", `${CLUSTER_PATH}/rotate`)).toBe(1));
    await waitFor(() => expect(countTo(calls, "GET", `${CLUSTER_PATH}/credentials`)).toBe(1));
    expect(await screen.findByText("dbx_2_new")).toBeInTheDocument();
  });

  it("shows the server's reason when a rotation is refused", async () => {
    installFetch(
      base({
        [`GET ${CLUSTER_PATH}`]: { json: cluster() },
        [`POST ${CLUSTER_PATH}/rotate`]: codedError(
          "cluster_unavailable",
          "the cluster is not ready",
          503,
          "wait for it to report ready",
        ),
      }),
    );
    renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });

    const dialog = await clickIn("Rotate");
    await userEvent.click(within(dialog).getByRole("button", { name: "Rotate" }));
    expect(
      await inPage("the cluster is not ready (wait for it to report ready)"),
    ).toBeInTheDocument();
  });
});

describe("Cluster deletion", () => {
  it("tells the operator what is happening and refreshes the list", async () => {
    let deleting = false;
    const calls = installFetch(
      base({
        "GET /v1/clusters": () => ({ json: { clusters: [cluster()] } }),
        [`GET ${CLUSTER_PATH}`]: () => ({
          json: cluster({ status: deleting ? "deleting" : "ready" }),
        }),
        [`DELETE ${CLUSTER_PATH}`]: () => {
          deleting = true;
          return { json: cluster({ status: "deleting" }) };
        },
      }),
    );
    renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });

    const dialog = await clickIn("Delete cluster");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete cluster" }));

    await waitFor(() => expect(countTo(calls, "DELETE", CLUSTER_PATH)).toBe(1));
    expect(await screen.findAllByText("deleting")).toHaveLength(2);
  });

  it("explains a refusal to delete a cluster that still has workers", async () => {
    installFetch(
      base({
        [`GET ${CLUSTER_PATH}`]: { json: cluster() },
        [`DELETE ${CLUSTER_PATH}`]: codedError(
          "cluster_has_nodes",
          "the cluster still has 2 workers attached",
          409,
        ),
      }),
    );
    renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });

    const dialog = await clickIn("Delete cluster");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete cluster" }));
    expect(await inPage("the cluster still has 2 workers attached")).toBeInTheDocument();
  });

  it("hides the delete action once the cluster is gone", async () => {
    installFetch(base({ [`GET ${CLUSTER_PATH}`]: { json: cluster({ status: "deleted" }) } }));
    renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });

    expect(await screen.findAllByText("deleted")).toHaveLength(2);
    expect(screen.queryByRole("button", { name: "Delete cluster" })).not.toBeInTheDocument();
  });
});

describe("Worker nodes", () => {
  const nodeRoutes = (extra: Routes = {}): Routes =>
    base({
      [`GET ${CLUSTER_PATH}`]: { json: cluster() },
      "GET /v1/providers/aws/instance-types": CATALOGUE,
      [`GET ${NODES_PATH}`]: { json: { nodes: [] } },
      ...extra,
    });
  const at = (extra: Routes = {}) => {
    installFetch(nodeRoutes(extra));
    renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });
  };

  it("adds a worker and shows it provisioning", async () => {
    const calls = installFetch(
      nodeRoutes({
        [`GET ${NODES_PATH}`]: { json: { nodes: [worker({ status: "provisioning" })] } },
        [`POST ${NODES_PATH}`]: { json: worker({ status: "provisioning" }) },
      }),
    );
    renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });

    await userEvent.click(await screen.findByLabelText("worker size"));
    await userEvent.click(await screen.findByRole("option", { name: /^t4g\.medium/ }));
    await userEvent.click(screen.getByRole("button", { name: "Add worker" }));

    await waitFor(() => expect(countTo(calls, "POST", NODES_PATH)).toBe(1));
    expect(posted(calls, "POST", NODES_PATH)).toEqual({
      instance_type: "t4g.medium",
      disk_gib: 30,
    });
    expect(await screen.findByText("provisioning")).toBeInTheDocument();
  });

  it("will not add a worker without a size", async () => {
    at({});
    expect(await screen.findByRole("button", { name: "Add worker" })).toBeDisabled();
  });

  it("explains a cluster that cannot take a worker yet", async () => {
    at({
      [`POST ${NODES_PATH}`]: codedError("cluster_unavailable", "the cluster is not ready", 503),
    });

    await userEvent.click(await screen.findByLabelText("worker size"));
    await userEvent.click(await screen.findByRole("option", { name: /^t4g\.medium/ }));
    await userEvent.click(screen.getByRole("button", { name: "Add worker" }));

    expect(await inPage(/The cluster is not ready to take a worker/)).toBeInTheDocument();
  });

  it("shows the server's reason when an add is refused for another reason", async () => {
    at({ [`POST ${NODES_PATH}`]: apiError("invalid_request", "disk is below the minimum", 400) });

    await userEvent.click(await screen.findByLabelText("worker size"));
    await userEvent.click(await screen.findByRole("option", { name: /^t4g\.medium/ }));
    await userEvent.click(screen.getByRole("button", { name: "Add worker" }));

    expect(await inPage("invalid_request (disk is below the minimum)")).toBeInTheDocument();
  });

  it("refuses a removal while the worker holds sandboxes and says how many", async () => {
    const calls = installFetch(
      nodeRoutes({ [`GET ${NODES_PATH}`]: { json: { nodes: [worker({ sandboxes: 2 })] } } }),
    );
    renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });

    expect(await screen.findByText("holds 2 sandboxes")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    expect(countTo(calls, "DELETE", `${NODES_PATH}/w-1`)).toBe(0);
  });

  it("uses the singular for a worker holding one sandbox", async () => {
    at({ [`GET ${NODES_PATH}`]: { json: { nodes: [worker({ sandboxes: 1 })] } } });
    expect(await screen.findByText("holds 1 sandbox")).toBeInTheDocument();
  });

  it("explains a refusal from the cluster even when the panel thought the worker was free", async () => {
    installFetch(
      nodeRoutes({
        [`GET ${NODES_PATH}`]: { json: { nodes: [worker()] } },
        [`DELETE ${NODES_PATH}/w-1`]: codedError(
          "node_holds_sandboxes",
          "the worker still holds 3 sandboxes",
          409,
        ),
      }),
    );
    renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });

    const dialog = await clickIn("Remove");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));

    expect(await inPage(/Still holding sandboxes, so the removal was refused/)).toBeInTheDocument();
  });

  it("removes a worker that holds nothing", async () => {
    const calls = installFetch(
      nodeRoutes({
        [`GET ${NODES_PATH}`]: { json: { nodes: [worker()] } },
        [`DELETE ${NODES_PATH}/w-1`]: { status: 204 },
      }),
    );
    renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });

    const dialog = await clickIn("Remove");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(countTo(calls, "DELETE", `${NODES_PATH}/w-1`)).toBe(1));
    await waitFor(() => expect(countTo(calls, "GET", NODES_PATH)).toBe(2));
  });

  it("shows a worker's own status and diagnostic", async () => {
    at({
      [`GET ${NODES_PATH}`]: {
        json: { nodes: [worker({ status: "failed", detail: "image pull backoff" })] },
      },
    });
    expect(await screen.findByText("failed")).toBeInTheDocument();
    expect(screen.getByText("image pull backoff")).toBeInTheDocument();
  });

  it("invites the operator to add one when the cluster has no workers", async () => {
    at({});
    expect(
      await screen.findByText("No workers yet; everything runs on the cluster host."),
    ).toBeInTheDocument();
  });

  it("shows the server's reason when the worker list cannot be read", async () => {
    at({
      [`GET ${NODES_PATH}`]: codedError("cluster_unavailable", "the cluster is not ready", 503),
    });
    expect(await inPage("the cluster is not ready")).toBeInTheDocument();
  });

  it("polls while a worker is moving and stops when they are all settled", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = installFetch(
        base({
          [`GET ${CLUSTER_PATH}`]: { json: cluster() },
          [`GET ${NODES_PATH}`]: { json: { nodes: [worker({ status: "removing" })] } },
        }),
      );
      renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(5000);
      expect(countTo(calls, "GET", NODES_PATH)).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("Cluster list", () => {
  it("opens the cluster an operator picks", async () => {
    installFetch(
      base({
        "GET /v1/clusters": {
          json: { clusters: [cluster(), cluster({ name: "spare", status: "failed" })] },
        },
        "GET /v1/clusters/spare": { json: cluster({ name: "spare", status: "failed" }) },
      }),
    );
    renderApp({ entry: "/clusters", me: ADMIN });

    await userEvent.click(await screen.findByRole("button", { name: /spare/ }));
    expect(await screen.findByRole("heading", { name: "spare" })).toBeInTheDocument();
  });

  it("invites the operator to create one when there are none", async () => {
    installFetch(base());
    renderApp({ entry: "/clusters", me: ADMIN });
    expect(await screen.findByText("No clusters yet.")).toBeInTheDocument();
  });

  it("shows the server's message instead of an empty list", async () => {
    installFetch(base({ "GET /v1/clusters": apiError("store offline", "retry in a minute", 503) }));
    renderApp({ entry: "/clusters", me: ADMIN });

    expect(await screen.findByText("store offline (retry in a minute)")).toBeInTheDocument();
    expect(screen.queryByText("No clusters yet.")).not.toBeInTheDocument();
  });

  it("polls while a cluster is provisioning", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = installFetch(
        base({ "GET /v1/clusters": { json: { clusters: [cluster({ status: "provisioning" })] } } }),
      );
      renderApp({ entry: "/clusters", me: ADMIN });
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(5000);
      expect(countTo(calls, "GET", "/v1/clusters")).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

// The page must render the shell's own surfaces without a cluster behind them.
describe("Reachable through renderApp", () => {
  it("renders the clusters page from the harness route tree", async () => {
    const reply: Reply = { json: { clusters: [] } };
    installFetch(base({ "GET /v1/clusters": reply }));
    renderApp({ entry: "/clusters", me: ADMIN });
    expect(await screen.findByRole("heading", { name: "Provider" })).toBeInTheDocument();
  });
});

describe("Moving around", () => {
  const configured = (extra: Routes = {}) =>
    base({
      "GET /v1/providers/aws/regions": { json: { regions: ["eu-west-1"] } },
      "GET /v1/providers/aws/instance-types": CATALOGUE,
      "POST /v1/providers/aws/estimate": { json: QUOTE },
      ...extra,
    });

  it("sends the chosen disk size to the estimate", async () => {
    const calls = installFetch(configured());
    renderApp({ entry: "/clusters?step=configure&provider=aws", me: ADMIN });

    await userEvent.type(await screen.findByLabelText("cluster name"), "boxy");
    await userEvent.click(screen.getByLabelText("region"));
    await userEvent.click(await screen.findByRole("option", { name: "eu-west-1" }));
    await userEvent.click(screen.getByLabelText("size"));
    await userEvent.click(await screen.findByRole("option", { name: /^t4g\.medium/ }));
    const disk = screen.getByLabelText("disk");
    await userEvent.clear(disk);
    await userEvent.type(disk, "120");
    await userEvent.click(screen.getByRole("button", { name: "Estimate price" }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/providers/aws/estimate")).toBe(1));
    expect(posted(calls, "POST", "/v1/providers/aws/estimate")).toEqual({
      region: "eu-west-1",
      instance_type: "t4g.medium",
      disk_gib: 120,
    });
  });

  it("shows the server's reason when the size catalogue cannot be read", async () => {
    installFetch(
      base({
        "GET /v1/providers/aws/regions": { json: { regions: ["eu-west-1"] } },
        "GET /v1/providers/aws/instance-types": apiError("forbidden", "admin only", 403),
      }),
    );
    renderApp({ entry: "/clusters?step=configure&provider=aws", me: ADMIN });

    expect(await inPage("forbidden (admin only)")).toBeInTheDocument();
    expect(screen.getByLabelText("size")).toBeInTheDocument();
  });

  it("goes back from the configuration to the provider list", async () => {
    installFetch(configured());
    renderApp({ entry: "/clusters?step=configure&provider=aws", me: ADMIN });

    await userEvent.click(await screen.findByRole("button", { name: "Back" }));
    expect(await screen.findByRole("heading", { name: "Provider" })).toBeInTheDocument();
  });

  it("sends an operator with no estimate to the configuration", async () => {
    installFetch(configured());
    renderApp({ entry: "/clusters?step=price&provider=aws", me: ADMIN });

    await userEvent.click(await screen.findByRole("button", { name: "Configure the cluster" }));
    expect(await screen.findByRole("heading", { name: "Configuration" })).toBeInTheDocument();
  });

  it("goes back from the price review to the configuration", async () => {
    const calls = installFetch(configured());
    renderApp({ entry: "/clusters", me: ADMIN });

    await toPrice();
    await userEvent.click(screen.getByRole("button", { name: "Change configuration" }));
    expect(await screen.findByRole("heading", { name: "Configuration" })).toBeInTheDocument();
    // Leaving the review is not creating anything.
    expect(countTo(calls, "POST", "/v1/clusters")).toBe(0);
  });

  it("opens the worker panel from the cluster and comes back", async () => {
    installFetch(
      base({
        [`GET ${CLUSTER_PATH}`]: { json: cluster() },
        [`GET ${NODES_PATH}`]: { json: { nodes: [] } },
      }),
    );
    renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });

    await userEvent.click(await screen.findByRole("button", { name: "Worker nodes" }));
    expect(await screen.findByRole("heading", { name: "Worker nodes" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Back to the cluster" }));
    expect(await screen.findByRole("heading", { name: "boxy" })).toBeInTheDocument();
  });

  it("sends the worker's chosen disk size with the add", async () => {
    const calls = installFetch(
      base({
        [`GET ${CLUSTER_PATH}`]: { json: cluster() },
        [`GET ${NODES_PATH}`]: { json: { nodes: [] } },
        "GET /v1/providers/aws/instance-types": CATALOGUE,
        [`POST ${NODES_PATH}`]: { json: worker({ status: "provisioning" }) },
      }),
    );
    renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });

    await userEvent.click(await screen.findByLabelText("worker size"));
    await userEvent.click(await screen.findByRole("option", { name: /^m7g\.large/ }));
    const disk = screen.getByLabelText("worker disk");
    await userEvent.clear(disk);
    await userEvent.type(disk, "60");
    await userEvent.click(screen.getByRole("button", { name: "Add worker" }));

    await waitFor(() => expect(countTo(calls, "POST", NODES_PATH)).toBe(1));
    expect(posted(calls, "POST", NODES_PATH)).toEqual({
      instance_type: "m7g.large",
      disk_gib: 60,
    });
  });

  it("shows the server's reason when a removal is refused for another reason", async () => {
    installFetch(
      base({
        [`GET ${CLUSTER_PATH}`]: { json: cluster() },
        [`GET ${NODES_PATH}`]: { json: { nodes: [worker()] } },
        [`DELETE ${NODES_PATH}/w-1`]: apiError("cluster_unavailable", "not reachable", 503),
      }),
    );
    renderApp({ entry: "/clusters?step=nodes&name=boxy", me: ADMIN });

    const dialog = await clickIn("Remove");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(await inPage("cluster_unavailable (not reachable)")).toBeInTheDocument();
  });

  it("copies the certificate pin to the clipboard", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    installFetch(
      base({
        [`GET ${CLUSTER_PATH}`]: { json: cluster({ tls_pin: "sha256/PIN" }) },
      }),
    );
    renderApp({ entry: "/clusters?step=status&name=boxy", me: ADMIN });

    await userEvent.click(await screen.findByRole("button", { name: "Copy" }));
    expect(writeText).toHaveBeenCalledWith("sha256/PIN");
  });
});
