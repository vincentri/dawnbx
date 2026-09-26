// @vitest-environment jsdom
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiError, countTo, installFetch, noSession, type Reply, releaseFetch } from "@/test-fetch";
import { ADMIN, ME, renderApp, STATUS } from "@/test-support";

afterEach(releaseFetch);

const SANDBOXES: Reply = {
  json: {
    sandboxes: [
      {
        id: "box-1",
        image: "python:3.12-slim",
        status: "running",
        network: "internet",
        created: "2026-09-26T10:00:00Z",
        expires_at: "2026-09-26T12:00:00Z",
        restarted_at: null,
        org: "acme",
      },
    ],
  },
};

const CONTROL_PLANE: Reply = { json: { control_plane: true, providers: ["aws"], version: "v1" } };
const CLUSTER: Reply = { json: { control_plane: false, providers: ["aws"], version: "v1" } };

const base = (me: Reply = { json: ME }): Record<string, Reply> => ({
  "GET /v1/me": me,
  "GET /v1/control-plane": CLUSTER,
  "GET /v1/status": { json: STATUS },
  "GET /v1/sandboxes": SANDBOXES,
});

const signIn = async () => {
  await userEvent.type(await screen.findByLabelText("Username"), "ada");
  await userEvent.type(screen.getByLabelText("Password"), "hunter2hunter2");
  await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
};

describe("Shell sign-in gate", () => {
  it("shows the sign-in form instead of the app when /v1/me has no dashboard user", async () => {
    const calls = installFetch(base({ json: { org: "acme", admin: false } })); // an API key: no `user`
    renderApp();

    expect(await screen.findByText("Sign in to your sandbox server.")).toBeInTheDocument();
    expect(screen.getByLabelText("Username")).toBeRequired();
    expect(screen.getByLabelText("Password")).toHaveAttribute("type", "password");
    expect(screen.queryByRole("link", { name: "Sandboxes" })).not.toBeInTheDocument();
    // No session, so the app never asks for the rest of the dashboard.
    expect(screen.queryByText(/disk free/)).not.toBeInTheDocument();
    expect(countTo(calls, "GET", "/v1/sandboxes")).toBe(0);
  });

  it("renders the app chrome once /v1/me resolves a user", async () => {
    installFetch(base());
    renderApp();

    expect(await screen.findByRole("link", { name: "Sandboxes" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Settings" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /ada/ })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(/42% disk free/)).toBeInTheDocument());
    expect(screen.getByText(/42% disk free/).parentElement).toHaveTextContent(
      "1.4.0 · 42% disk free · warm 1/4",
    );
  });

  it("marks a nearly full disk as destructive", async () => {
    installFetch({
      ...base(),
      "GET /v1/status": { json: { ...STATUS, free_pct: 9.2 } },
    });
    renderApp({ me: ME });

    const free = await screen.findByText(/9% disk free/);
    expect(free).toHaveClass("text-destructive");
  });

  it("signs in with the trimmed username and swaps the form for the chrome", async () => {
    const calls = installFetch({
      ...base({ json: { org: "acme", admin: false } }),
      "POST /v1/login": { json: ME },
    });
    renderApp();
    await signIn();

    await waitFor(() => expect(screen.getByRole("button", { name: /ada/ })).toBeInTheDocument());
    const login = calls.filter((c) => c.url.startsWith("/v1/login"));
    expect(login).toHaveLength(1);
    expect(login[0].body).toEqual({
      username: "ada",
      password: "hunter2hunter2",
    });
    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
  });

  it("keeps the username but drops the password when sign-in is rejected", async () => {
    const calls = installFetch({
      ...base({ json: { org: "acme", admin: false } }),
      "POST /v1/login": {
        status: 401,
        json: { error: { code: "bad", message: "wrong password" } },
      },
    });
    renderApp();
    await signIn();

    await waitFor(() => expect(screen.getByLabelText("Password")).toHaveValue(""));
    expect(screen.getByLabelText("Username")).toHaveValue("ada");
    expect(screen.getByRole("button", { name: "Sign in" })).toBeEnabled();
    // A rejected login is not a session loss, so nothing else is torn down.
    expect(countTo(calls, "GET", "/v1/sandboxes")).toBe(0);
  });
});

describe("Shell session loss", () => {
  it("drops back to sign-in and clears cached data when a call comes back 401", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let sandboxes: Reply = SANDBOXES;
      const calls = installFetch({
        ...base(),
        "GET /v1/sandboxes": () => sandboxes,
      });
      const { qc } = renderApp({ me: ME });

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByText("box-1")).toBeInTheDocument();
      expect(qc.getQueryData(["sandboxes"])).toBeDefined();

      sandboxes = noSession;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2000); // the 2s list poll is what notices
      });
      expect(countTo(calls, "GET", "/v1/sandboxes")).toBe(2);
      await waitFor(() =>
        expect(screen.getByText("Sign in to your sandbox server.")).toBeInTheDocument(),
      );
      expect(qc.getQueryData(["me"])).toBeNull();
      expect(qc.getQueryData(["sandboxes"])).toBeUndefined();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("Shell sign out", () => {
  it("posts to the logout route, shows sign-in and forgets the old user's data", async () => {
    const calls = installFetch({
      ...base(),
      "POST /v1/logout": { status: 204 },
    });
    const { qc } = renderApp({ me: ADMIN });

    await userEvent.click(await screen.findByRole("button", { name: /root/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: /Sign out/ }));

    await waitFor(() =>
      expect(screen.getByText("Sign in to your sandbox server.")).toBeInTheDocument(),
    );
    expect(countTo(calls, "POST", "/v1/logout")).toBe(1);
    expect(qc.getQueryData(["me"])).toBeNull();
    expect(qc.getQueryData(["sandboxes"])).toBeUndefined();
    expect(qc.getQueryData(["status"])).toBeUndefined();
  });

  it("labels the menu with the signed-in user's org and offers an account link", async () => {
    installFetch(base());
    renderApp({ me: ME });
    await userEvent.click(await screen.findByRole("button", { name: /ada/ }));

    expect(await screen.findByText("member of acme")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Account/ })).toHaveAttribute(
      "href",
      "/settings?tab=account",
    );
  });
});

describe("Shell status poll", () => {
  it("re-reads /v1/status every 5s", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = installFetch(base());
      renderApp({ me: ME });
      await vi.advanceTimersByTimeAsync(0);
      expect(countTo(calls, "GET", "/v1/status")).toBe(1);

      await vi.advanceTimersByTimeAsync(5000);
      expect(countTo(calls, "GET", "/v1/status")).toBe(2);
      await vi.advanceTimersByTimeAsync(5000);
      expect(countTo(calls, "GET", "/v1/status")).toBe(3);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("Shell capability probe", () => {
  it("offers the cluster wizard and keeps the sandbox surfaces on a cluster", async () => {
    installFetch(base());
    renderApp({ me: ME });

    expect(await screen.findByRole("link", { name: "Sandboxes" })).toHaveAttribute("href", "/");
    expect(screen.getByRole("link", { name: "Clusters" })).toHaveAttribute("href", "/clusters");
  });

  it("hides the sandbox surfaces and skips /v1/status on a control plane", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = installFetch({ ...base(), "GET /v1/control-plane": CONTROL_PLANE });
      renderApp({ me: ADMIN });
      await vi.advanceTimersByTimeAsync(10_000);

      expect(screen.queryByRole("link", { name: "Sandboxes" })).not.toBeInTheDocument();
      expect(screen.getByRole("link", { name: "Clusters" })).toHaveAttribute("href", "/clusters");
      // The brand goes where the dashboard's only subject is.
      expect(screen.getByRole("link", { name: "dawnbx" })).toHaveAttribute("href", "/clusters");
      // /v1/status is a cluster route; polling it here would be a 503 every 5s.
      expect(countTo(calls, "GET", "/v1/status")).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it("treats a server that cannot answer the probe as a cluster", async () => {
    installFetch({ ...base(), "GET /v1/control-plane": apiError("no such route", "", 404) });
    renderApp({ me: ME });

    expect(await screen.findByRole("link", { name: "Sandboxes" })).toBeInTheDocument();
  });
});
