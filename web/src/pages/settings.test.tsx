// @vitest-environment jsdom
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import {
  apiError,
  countTo,
  installFetch,
  type Reply,
  type Routes,
  releaseFetch,
} from "@/test-fetch";
import { ADMIN, ME, renderApp, STATUS } from "@/test-support";

afterEach(releaseFetch);

const KEY_ROWS = [
  {
    id: "k-old",
    org: "acme",
    name: "old",
    created: "2026-01-02T03:04:05Z",
    created_by: "ada",
    last_used: null,
    expires: null,
    revoked: null,
  },
  {
    id: "k-ci",
    org: "acme",
    name: "ci",
    created: "2026-05-06T07:08:09Z",
    created_by: "ada",
    last_used: "2026-09-01T00:00:00Z",
    expires: null,
    revoked: "2026-09-20T00:00:00Z",
  },
];

const base = (extra: Routes = {}) => ({
  "GET /v1/me": { json: ADMIN },
  "GET /v1/status": { json: STATUS },
  ...extra,
});

const openTab = async (tab: string) => {
  renderApp({ entry: `/settings?tab=${tab}`, me: ADMIN });
  return screen.findByRole("tab", { selected: true });
};

const tab = (name: string) => screen.findByRole("tab", { name });

/** Settings tables all live under <main>; the header repeats "acme" and "root". */
const rowOf = async (text: string) =>
  (await within(screen.getByRole("main")).findByText(text)).closest("tr")!;

describe("Settings shell", () => {
  it("hides the admin tabs from a plain member", async () => {
    installFetch({
      "GET /v1/me": { json: ME },
      "GET /v1/status": { json: STATUS },
      "GET /v1/keys": { json: { keys: [] } },
    });
    renderApp({ entry: "/settings?tab=keys", me: ME });

    expect(await tab("API keys")).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Users" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Orgs" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Nodes" })).not.toBeInTheDocument();
  });

  it("navigates between tabs and loads that tab's data", async () => {
    const calls = installFetch(
      base({
        "GET /v1/keys": { json: { keys: [] } },
        "GET /v1/audit": { json: { events: [] } },
      }),
    );
    await openTab("keys");
    expect(await screen.findByText("No keys yet.")).toBeInTheDocument();

    await userEvent.click(await tab("Audit log"));

    expect(await screen.findByText("Nothing yet.")).toBeInTheDocument();
    expect(countTo(calls, "GET", "/v1/audit")).toBe(1);
  });
});

describe("API keys", () => {
  it("lists keys newest first and shows a revoked one as spent", async () => {
    installFetch(base({ "GET /v1/keys": { json: { keys: KEY_ROWS } } }));
    await openTab("keys");

    const table = (await screen.findByText("k-ci")).closest("table")!;
    const rows = within(table).getAllByRole("row");
    // The newest key is on top.
    expect(within(rows[1]).getByText("k-ci")).toBeInTheDocument();
    expect(within(rows[2]).getByText("k-old")).toBeInTheDocument();
    expect(within(rows[1]).getByText("revoked")).toBeInTheDocument();
    expect(within(rows[1]).queryByRole("button", { name: "Revoke" })).not.toBeInTheDocument();
    // A live key is revokable, and an unexpiring one says so.
    expect(within(rows[2]).getByRole("button", { name: "Revoke" })).toBeInTheDocument();
    expect(within(rows[2]).getByText("never")).toBeInTheDocument();
    expect(within(rows[2]).getByText("–")).toBeInTheDocument(); // never used
  });

  it("creates a key and shows the secret exactly once", async () => {
    const calls = installFetch(
      base({
        "GET /v1/keys": { json: { keys: [] } },
        "POST /v1/keys": { json: { key: "dnbx_live_abc123", id: "k-new" } },
      }),
    );
    await openTab("keys");
    await screen.findByText("No keys yet.");

    await userEvent.type(screen.getByPlaceholderText("name, e.g. ci"), "ci");
    await userEvent.click(screen.getByLabelText("expires"));
    await userEvent.click(await screen.findByRole("option", { name: "30 days" }));
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));

    expect(await screen.findByText("dnbx_live_abc123")).toBeInTheDocument();
    // An admin's key always names the org it belongs to.
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      name: "ci",
      ttl: "720h",
      org: "acme",
    });
    expect(screen.getByPlaceholderText("name, e.g. ci")).toHaveValue("");
  });

  it("leaves ttl out entirely for a key that never expires", async () => {
    const calls = installFetch(
      base({
        "GET /v1/keys": { json: { keys: [] } },
        "POST /v1/keys": { json: { key: "dnbx_live_x", id: "k" } },
      }),
    );
    await openTab("keys");
    await screen.findByText("No keys yet.");

    await userEvent.type(screen.getByPlaceholderText("name, e.g. ci"), "forever");
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/keys")).toBe(1));
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      name: "forever",
      org: "acme",
    });
  });

  it("lets an admin pick the key's org", async () => {
    const calls = installFetch(
      base({
        "GET /v1/keys": { json: { keys: [] } },
        "GET /v1/orgs": {
          json: {
            orgs: [{ id: "acme", name: "Acme", created: "2026-01-01T00:00:00Z" }],
          },
        },
      }),
    );
    await openTab("keys");
    await screen.findByText("No keys yet.");

    await userEvent.type(screen.getByPlaceholderText("name, e.g. ci"), "ci");
    await userEvent.click(await screen.findByLabelText("org"));
    await userEvent.click(await screen.findByRole("option", { name: "acme" }));
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/keys")).toBe(1));
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      name: "ci",
      org: "acme",
    });
  });

  it("confirms before revoking, and deletes the key only then", async () => {
    const calls = installFetch(
      base({
        "GET /v1/keys": { json: { keys: KEY_ROWS } },
        "DELETE /v1/keys/{key}": { status: 204 },
      }),
    );
    await openTab("keys");
    await userEvent.click(await screen.findByRole("button", { name: "Revoke" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText('Revoke "old"?')).toBeInTheDocument();
    expect(
      within(dialog).getByText("Clients using it stop working within 30 seconds."),
    ).toBeInTheDocument();
    expect(countTo(calls, "DELETE", "/v1/keys/k-old")).toBe(0);

    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(countTo(calls, "DELETE", "/v1/keys/k-old")).toBe(1));
  });

  it("disables Create key while the request is in flight", async () => {
    let release!: (r: Reply) => void;
    const gate = new Promise<Reply>((r) => {
      release = r;
    });
    installFetch(
      base({
        "GET /v1/keys": { json: { keys: [] } },
        "POST /v1/keys": () => gate,
      }),
    );
    await openTab("keys");
    await screen.findByText("No keys yet.");

    await userEvent.type(screen.getByPlaceholderText("name, e.g. ci"), "ci");
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    expect(await screen.findByRole("button", { name: "Create key" })).toBeDisabled();

    release({ json: { key: "dnbx_live_x", id: "k" } });
    await waitFor(() => expect(screen.getByRole("button", { name: "Create key" })).toBeEnabled());
  });
});

describe("Audit log", () => {
  it("renders one row per event", async () => {
    installFetch(
      base({
        "GET /v1/audit": {
          json: {
            events: [
              {
                at: "2026-09-25T10:00:00Z",
                org: "acme",
                actor: "ada",
                action: "sandbox.create",
                target: "box-1",
              },
            ],
          },
        },
      }),
    );
    await openTab("audit");

    const row = await rowOf("sandbox.create");
    expect(within(row).getByText("ada")).toBeInTheDocument();
    expect(within(row).getByText("acme")).toBeInTheDocument();
    expect(within(row).getByText("box-1")).toBeInTheDocument();
    expect(within(row).getByText(/9\/25\/2026/)).toBeInTheDocument();
  });
});

describe("Users", () => {
  const users = [
    {
      id: "u1",
      org: "acme",
      username: "root",
      role: "admin",
      created: "2026-01-01T00:00:00Z",
    },
    {
      id: "u2",
      org: "acme",
      username: "ada",
      role: "member",
      created: "2026-02-02T00:00:00Z",
    },
  ];

  it("marks your own row and offers delete on everyone else's", async () => {
    installFetch(base({ "GET /v1/users": { json: { users } } }));
    await openTab("users");

    const mine = await rowOf("root");
    expect(within(mine).getByText("you")).toBeInTheDocument();
    expect(within(mine).queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
    const theirs = await rowOf("ada");
    expect(within(theirs).getByRole("button", { name: "Delete" })).toBeInTheDocument();
    expect(within(theirs).getByRole("button", { name: "Reset password" })).toBeInTheDocument();
  });

  it("adds a user with a trimmed username and the chosen role", async () => {
    const calls = installFetch(
      base({
        "GET /v1/users": { json: { users } },
        "GET /v1/orgs": {
          json: {
            orgs: [{ id: "acme", name: "Acme", created: "2026-01-01T00:00:00Z" }],
          },
        },
        "POST /v1/users": { status: 201, json: { id: "u3" } },
      }),
    );
    await openTab("users");
    await screen.findByText("root");

    await userEvent.type(screen.getByPlaceholderText("username"), "  bob  ");
    await userEvent.type(screen.getByPlaceholderText("password (10+ chars)"), "correcthorseb");
    await userEvent.click(screen.getByLabelText("role"));
    await userEvent.click(await screen.findByRole("option", { name: "admin" }));
    await userEvent.click(screen.getByRole("button", { name: "Add user" }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/users")).toBe(1));
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      username: "bob",
      password: "correcthorseb",
      org: "acme",
      role: "admin",
    });
    expect(screen.getByPlaceholderText("username")).toHaveValue("");
  });

  it("confirms a delete and only then removes the user", async () => {
    const calls = installFetch(
      base({
        "GET /v1/users": { json: { users } },
        "DELETE /v1/users/{username}": { status: 204 },
      }),
    );
    await openTab("users");
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Delete user ada?")).toBeInTheDocument();
    expect(
      within(dialog).getByText("Their sessions end now. Their API keys keep working."),
    ).toBeInTheDocument();
    expect(countTo(calls, "DELETE", "/v1/users/ada")).toBe(0);

    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(countTo(calls, "DELETE", "/v1/users/ada")).toBe(1));
  });

  it("sets another user's password through the dialog and confirms it with a toast", async () => {
    const calls = installFetch(
      base({
        "GET /v1/users": { json: { users } },
        "POST /v1/users/ada/password": { status: 204 },
      }),
    );
    await openTab("users");
    // root is you, so only ada's row offers the action.
    await userEvent.click(await screen.findByRole("button", { name: "Reset password" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("New password for ada")).toBeInTheDocument();
    await userEvent.type(within(dialog).getByPlaceholderText("10 to 72 chars"), "brandnewsecret");
    await userEvent.click(within(dialog).getByRole("button", { name: "Set password" }));

    expect(await screen.findByText("Password for ada changed")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      password: "brandnewsecret",
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("reports a rejected user creation as a toast and keeps the form", async () => {
    installFetch(
      base({
        "GET /v1/users": { json: { users } },
        "POST /v1/users": apiError("username taken", "pick another", 409),
      }),
    );
    await openTab("users");
    await screen.findByText("root");

    await userEvent.type(screen.getByPlaceholderText("username"), "bob");
    await userEvent.type(screen.getByPlaceholderText("password (10+ chars)"), "correcthorseb");
    await userEvent.click(screen.getByRole("button", { name: "Add user" }));

    expect(await screen.findByText("username taken (pick another)")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("username")).toHaveValue("bob");
  });
});

describe("Orgs", () => {
  it("lists orgs, marking the built-in one with a dash", async () => {
    installFetch(
      base({
        "GET /v1/orgs": {
          json: {
            orgs: [
              { id: "acme", name: "Acme Inc", created: "2026-03-04T00:00:00Z" },
              { id: "default", name: "", created: "1970-01-01T00:00:00Z" },
            ],
          },
        },
      }),
    );
    await openTab("orgs");

    const named = await rowOf("acme");
    expect(within(named).getByText("Acme Inc")).toBeInTheDocument();
    expect(within(named).getByText(/3\/4\/2026/)).toBeInTheDocument();
    expect(within(await rowOf("default")).getByText("–")).toBeInTheDocument();
  });

  it("creates an org from a trimmed id and clears the form", async () => {
    const calls = installFetch(
      base({
        "GET /v1/orgs": { json: { orgs: [] } },
        "POST /v1/orgs": { status: 201, json: { id: "globex" } },
      }),
    );
    await openTab("orgs");
    await screen.findByRole("button", { name: "Add org" });

    await userEvent.type(screen.getByPlaceholderText("id, e.g. acme"), "globex");
    await userEvent.type(screen.getByPlaceholderText("display name (optional)"), "Globex");
    await userEvent.click(screen.getByRole("button", { name: "Add org" }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/orgs")).toBe(1));
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      id: "globex",
      name: "Globex",
    });
    expect(screen.getByPlaceholderText("id, e.g. acme")).toHaveValue("");
  });
});

describe("Nodes", () => {
  const nodes = [
    {
      name: "local",
      role: "server",
      ready: true,
      since: "2026-09-01T00:00:00Z",
      heartbeat: "2026-09-26T00:00:00Z",
      ip: "10.0.0.1",
      kubelet: "v1.31.2",
      sandboxes: 3,
    },
    {
      name: "worker-1",
      role: "worker",
      ready: false,
      since: "2026-09-20T00:00:00Z",
      heartbeat: "2026-09-26T00:00:00Z",
      ip: "10.0.0.9",
      kubelet: "v1.31.2",
      sandboxes: 1,
    },
  ];

  it("shows readiness per node and only offers removal for workers", async () => {
    installFetch(base({ "GET /v1/nodes": { json: { nodes } } }));
    await openTab("nodes");

    const server = await rowOf("local");
    expect(within(server).getByText("Ready")).toBeInTheDocument();
    expect(within(server).queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    const worker = await rowOf("worker-1");
    expect(within(worker).getByText(/NotReady since/)).toBeInTheDocument();
    expect(within(worker).getByRole("button", { name: "Remove" })).toBeInTheDocument();
  });

  it("says everything runs here when there is no worker", async () => {
    installFetch(base({ "GET /v1/nodes": { json: { nodes: [nodes[0]] } } }));
    await openTab("nodes");

    expect(
      await screen.findByText("No workers yet; everything runs on this server."),
    ).toBeInTheDocument();
  });

  it("reveals the join command when asked", async () => {
    const calls = installFetch(
      base({
        "GET /v1/nodes": { json: { nodes } },
        "GET /v1/nodes/join": {
          json: { command: "curl https://dawnbx/install.sh | sh -s worker-1" },
        },
      }),
    );
    await openTab("nodes");
    expect(screen.queryByText(/curl https/)).not.toBeInTheDocument();

    await userEvent.click(await screen.findByRole("button", { name: "Show join command" }));

    expect(
      await screen.findByText("curl https://dawnbx/install.sh | sh -s worker-1"),
    ).toBeInTheDocument();
    expect(countTo(calls, "GET", "/v1/nodes/join")).toBe(1);
  });

  it("confirms a node removal before deleting it", async () => {
    const calls = installFetch(
      base({
        "GET /v1/nodes": { json: { nodes } },
        "DELETE /v1/nodes/{name}": { status: 204 },
      }),
    );
    await openTab("nodes");
    await userEvent.click(await screen.findByRole("button", { name: "Remove" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Remove node worker-1?")).toBeInTheDocument();
    expect(
      within(dialog).getByText("Also run k3s-agent-uninstall.sh on it, or it rejoins."),
    ).toBeInTheDocument();
    expect(countTo(calls, "DELETE", "/v1/nodes/worker-1")).toBe(0);

    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(countTo(calls, "DELETE", "/v1/nodes/worker-1")).toBe(1));
  });
});

describe("Account", () => {
  it("changes the password, then signs out because the server ended every session", async () => {
    const calls = installFetch(base({ "POST /v1/me/password": { status: 204 } }));
    const { qc } = renderApp({ entry: "/settings?tab=account", me: ME });
    await screen.findByPlaceholderText("current password");

    await userEvent.type(screen.getByPlaceholderText("current password"), "oldsecret12");
    await userEvent.type(screen.getByPlaceholderText("new password (10+ chars)"), "newsecret12");
    await userEvent.click(screen.getByRole("button", { name: "Change password" }));

    expect(
      await screen.findByText("Password changed. Sign in with the new one."),
    ).toBeInTheDocument();
    expect(await screen.findByText("Sign in to your sandbox server.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      old: "oldsecret12",
      new: "newsecret12",
    });
    expect(qc.getQueryData(["me"])).toBeNull();
  });

  it("reports a wrong current password without signing out", async () => {
    const calls = installFetch(
      base({
        "POST /v1/me/password": apiError("wrong password", undefined, 403),
      }),
    );
    const { qc } = renderApp({ entry: "/settings?tab=account", me: ME });
    await screen.findByPlaceholderText("current password");

    await userEvent.type(screen.getByPlaceholderText("current password"), "nope");
    await userEvent.type(screen.getByPlaceholderText("new password (10+ chars)"), "newsecret12");
    await userEvent.click(screen.getByRole("button", { name: "Change password" }));

    expect(await screen.findByText("wrong password")).toBeInTheDocument();
    expect(countTo(calls, "POST", "/v1/me/password")).toBe(1);
    expect(qc.getQueryData(["me"])).toEqual(ME);
  });
});

// --- control-plane mode -------------------------------------------------------

// A control plane has no cluster, so the Nodes tab has nothing to report. It
// used to be offered anyway, poll a route that answers 503, and render an empty
// grid with no error — which reads as "no workers" rather than "there is no
// cluster here".
describe("Settings on a control plane", () => {
  const withProbe = (controlPlane: boolean) => {
    const calls = installFetch({
      "GET /v1/me": { json: ADMIN },
      "GET /v1/control-plane": {
        json: { control_plane: controlPlane, providers: ["aws"], version: "v1" },
      },
      "GET /v1/keys": { json: { keys: [] } },
    });
    return { calls, ...renderApp({ entry: "/settings", me: ADMIN }) };
  };

  it("does not offer the cluster's own worker tab", async () => {
    const { calls } = withProbe(true);
    await waitFor(() =>
      expect(screen.queryByRole("tab", { name: "Nodes" })).not.toBeInTheDocument(),
    );
    // And it never asks, so nothing polls a route that cannot answer.
    expect(countTo(calls, "GET", "/v1/nodes")).toBe(0);
  });

  it("offers it on a cluster, where it is the point of the tab", async () => {
    withProbe(false);
    await waitFor(() => expect(screen.getByRole("tab", { name: "Nodes" })).toBeInTheDocument());
  });
});
