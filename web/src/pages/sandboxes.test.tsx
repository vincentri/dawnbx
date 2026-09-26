// @vitest-environment jsdom
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  apiError,
  countTo,
  type FetchCall,
  installFetch,
  type Reply,
  type Routes,
  releaseFetch,
} from "@/test-fetch";
import { ME, renderApp, STATUS } from "@/test-support";

afterEach(releaseFetch);

// Both timestamps sit a minute either side of ninety minutes, not exactly on
// it. dur() floors to whole minutes, and the fixture is stamped before the
// component renders, so "exactly 90 minutes" makes time-left read 1h 29m as
// soon as a single second passes - which is most runs, and the ones that
// happened to land in the same millisecond passed by luck. A minute of
// cushion on both sides makes the floored value the same whatever the render
// costs, without freezing the clock and breaking the async queries.
const NINETY = 90 * 60_000 + 30_000;

const box = (over: Record<string, unknown> = {}) => ({
  id: "box-1",
  image: "python:3.12-slim",
  status: "running",
  network: "internet",
  created: new Date(Date.now() - NINETY).toISOString(),
  expires_at: new Date(Date.now() + NINETY).toISOString(),
  restarted_at: null,
  org: "acme",
  ...over,
});

const list = (sandboxes: unknown[]) => ({ json: { sandboxes } });

/** The page's own background routes; tests add whatever the click needs. */
const shell = (sandboxes: unknown[] = [box()], extra: Routes = {}) => ({
  "GET /v1/me": { json: ME },
  "GET /v1/status": { json: STATUS },
  "GET /v1/sandboxes": list(sandboxes),
  ...extra,
});

/** `ls` for the file browser, keyed off the shell-quoted cwd in the exec body. */
const lsFor = (dirs: Record<string, Reply>) => (c: FetchCall) => {
  const m = /ls -1Ap -- '(.*)'$/.exec((c.body as { cmd: string }).cmd);
  return dirs[m?.[1] ?? "."] ?? { json: { exit_code: 0, stdout: "", stderr: "" } };
};

const execPath = (id: string) => `/v1/sandboxes/${id}/exec`;
const toPath = (url: string) => new URL(url, "http://x").pathname;

/** Every command exec'd against a sandbox, the `ls` listings included. */
const execCmds = (calls: FetchCall[], id: string): string[] =>
  calls
    .filter((c) => c.method === "POST" && toPath(c.url) === execPath(id))
    .map((c) => (c.body as { cmd: string }).cmd);

const openBox = async (id = "box-1") => {
  renderApp({ entry: `/?id=${id}`, me: ME });
  return screen.findByText(id, { selector: "aside span" });
};

const detailOpen = (id = "box-1") => screen.queryByText(id, { selector: "aside span" });

describe("Sandbox list", () => {
  it("renders a row per sandbox with its status, image, network, age and expiry", async () => {
    installFetch(
      shell([
        box(),
        box({
          id: "box-2",
          status: "stopped",
          image: "ubuntu:24.04",
          network: "none",
          parent: "box-1",
        }),
      ]),
    );
    renderApp({ me: ME });

    const table = await screen.findByRole("table");
    const [, first, second] = within(table).getAllByRole("row");
    expect(within(first).getByText("box-1")).toBeInTheDocument();
    expect(within(first).getByText("running")).toBeInTheDocument();
    expect(within(first).getByText("python:3.12-slim")).toBeInTheDocument();
    expect(within(first).getByText("internet")).toBeInTheDocument();
    expect(within(first).getAllByText("1h 30m")).toHaveLength(2); // age, and time left
    expect(within(second).getByText("box-2")).toBeInTheDocument();
    expect(within(second).getByText("stopped")).toBeInTheDocument();
    expect(within(second).getByText("none")).toBeInTheDocument();
    // box-2 was forked from box-1, and the parent is a link, not dead text.
    expect(within(second).getByRole("link", { name: "box-1" })).toHaveAttribute(
      "href",
      "/?id=box-1",
    );
    // Every sandbox is on one node, so the table spends no column on it.
    expect(screen.queryByRole("columnheader", { name: "Node" })).not.toBeInTheDocument();
  });

  it("adds a Node column once sandboxes span nodes and spells out a never-expiring one", async () => {
    installFetch(
      shell([box({ node: "local" }), box({ id: "box-2", node: "worker-1", expires_at: null })]),
    );
    renderApp({ me: ME });

    const table = await screen.findByRole("table");
    const [, first, second] = within(table).getAllByRole("row");
    expect(within(table).getByRole("columnheader", { name: "Node" })).toBeInTheDocument();
    expect(within(first).getByText("local")).toBeInTheDocument();
    expect(within(second).getByText("worker-1")).toBeInTheDocument();
    expect(within(second).getByText("never")).toBeInTheDocument();
  });

  it("invites the user to create one when the list is empty", async () => {
    installFetch(shell([]));
    renderApp({ me: ME });

    expect(await screen.findByText("No sandboxes yet. Create one above.")).toBeInTheDocument();
  });

  it("shows the server's message when the list cannot be read", async () => {
    installFetch(
      shell([], {
        "GET /v1/sandboxes": apiError("sandbox store is offline", "retry in a minute", 503),
      }),
    );
    renderApp({ me: ME });

    expect(
      await screen.findByText("sandbox store is offline (retry in a minute)"),
    ).toBeInTheDocument();
  });

  it("opens the detail pane for the sandbox named in the URL", async () => {
    installFetch(shell([box()]));
    await openBox();

    expect(await screen.findByRole("link", { name: /Terminal/ })).toHaveAttribute(
      "href",
      "/terminal/box-1",
    );
    expect(screen.getByRole("button", { name: "Keep forever" })).toBeInTheDocument();
  });

  it("re-reads the list every 2s", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = installFetch(shell([box()]));
      renderApp({ me: ME });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(countTo(calls, "GET", "/v1/sandboxes")).toBe(1);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2000);
      });
      expect(countTo(calls, "GET", "/v1/sandboxes")).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("Create sandbox", () => {
  const create = box({ id: "box-new" });

  it("posts only the network when image and ttl are left blank", async () => {
    const calls = installFetch(shell([], { "POST /v1/sandboxes": { json: create } }));
    renderApp({ me: ME });

    await userEvent.click(await screen.findByRole("button", { name: /New sandbox/ }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/sandboxes")).toBe(1));
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      network: "internet",
    });
  });

  it("sends ttl: null for keep-forever, and the trimmed duration otherwise", async () => {
    const calls = installFetch(shell([], { "POST /v1/sandboxes": { json: create } }));
    renderApp({ me: ME });
    const posted = () => calls.filter((c) => c.method === "POST" && c.url === "/v1/sandboxes");

    const ttl = await screen.findByLabelText("ttl");
    await userEvent.type(ttl, "forever");
    await userEvent.click(screen.getByRole("button", { name: /New sandbox/ }));
    await waitFor(() => expect(posted()).toHaveLength(1));
    expect(posted()[0].body).toEqual({ network: "internet", ttl: null });

    await userEvent.clear(ttl);
    await userEvent.type(ttl, "2h");
    await userEvent.type(screen.getByLabelText("image"), "  ubuntu:24.04  ");
    await userEvent.click(screen.getByRole("button", { name: /New sandbox/ }));
    await waitFor(() => expect(posted()).toHaveLength(2));
    expect(posted()[1].body).toEqual({
      network: "internet",
      image: "ubuntu:24.04",
      ttl: "2h",
    });
  });

  it("posts network: none when the picker is switched off the internet", async () => {
    const calls = installFetch(shell([], { "POST /v1/sandboxes": { json: create } }));
    renderApp({ me: ME });

    await userEvent.click(await screen.findByLabelText("network"));
    await userEvent.click(await screen.findByRole("option", { name: "no network" }));
    await userEvent.click(screen.getByRole("button", { name: /New sandbox/ }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/sandboxes")).toBe(1));
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      network: "none",
    });
  });

  it("disables the button and shows Creating… while the POST is in flight", async () => {
    let release!: (r: Reply) => void;
    const gate = new Promise<Reply>((r) => {
      release = r;
    });
    installFetch(shell([], { "POST /v1/sandboxes": () => gate }));
    renderApp({ me: ME });

    await userEvent.click(await screen.findByRole("button", { name: /New sandbox/ }));
    expect(await screen.findByRole("button", { name: /Creating/ })).toBeDisabled();

    release({ json: create });
    await waitFor(() => expect(screen.getByRole("button", { name: /New sandbox/ })).toBeEnabled());
  });

  it("selects the new sandbox so its detail opens straight away", async () => {
    let live = [box()];
    installFetch(
      shell([], {
        "GET /v1/sandboxes": () => list(live),
        "POST /v1/sandboxes": () => {
          live = [...live, create];
          return { json: create };
        },
      }),
    );
    renderApp({ me: ME });

    await userEvent.click(await screen.findByRole("button", { name: /New sandbox/ }));
    expect(await screen.findByText("box-new", { selector: "aside span" })).toBeInTheDocument();
  });
});

describe("Sandbox detail actions", () => {
  const withLs = (extra: Routes) =>
    shell([box()], {
      "POST /v1/sandboxes/box-1/exec": lsFor({
        ".": { json: { exit_code: 0, stdout: "", stderr: "" } },
      }),
      ...extra,
    });

  it("runs a command and shows its stdout, stderr and exit code", async () => {
    const calls = installFetch(
      withLs({
        "POST /v1/sandboxes/box-1/exec": (c) =>
          (c.body as { cmd: string }).cmd === "cat nope"
            ? {
                json: {
                  exit_code: 2,
                  stdout: "hello\n",
                  stderr: "no such file\n",
                },
              }
            : { json: { exit_code: 0, stdout: "", stderr: "" } },
      }),
    );
    await openBox();

    await userEvent.type(await screen.findByLabelText("command"), "cat nope");
    await userEvent.click(screen.getByRole("button", { name: "Run" }));

    const out = await screen.findByText(/hello/);
    expect(out).toHaveTextContent("$ cat nope");
    expect(out).toHaveTextContent("hello");
    expect(out).toHaveTextContent("no such file");
    expect(out).toHaveTextContent("exit 2");
    expect(execCmds(calls, "box-1")).toContain("cat nope");
  });

  it("does not exec a blank command", async () => {
    const calls = installFetch(withLs({}));
    await openBox();
    await screen.findByRole("button", { name: "Run" });

    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    // Only the file browser's own listing, no user command.
    expect(execCmds(calls, "box-1")).toEqual(["ls -1Ap -- '.'"]);
  });

  it("extends by an hour, or pins the sandbox forever", async () => {
    const calls = installFetch(
      shell([box()], { "POST /v1/sandboxes/box-1/extend": { json: box() } }),
    );
    await openBox();

    await userEvent.click(await screen.findByRole("button", { name: /Extend 1h/ }));
    await waitFor(() => expect(countTo(calls, "POST", "/v1/sandboxes/box-1/extend")).toBe(1));
    expect(calls.filter((c) => c.url === "/v1/sandboxes/box-1/extend")[0].body).toEqual({
      ttl: "1h",
    });

    await userEvent.click(screen.getByRole("button", { name: "Keep forever" }));
    await waitFor(() => expect(countTo(calls, "POST", "/v1/sandboxes/box-1/extend")).toBe(2));
    expect(calls.filter((c) => c.url === "/v1/sandboxes/box-1/extend")[1].body).toEqual({
      ttl: null,
    });
  });

  it("starts a stopped sandbox, which hides the running-only controls", async () => {
    const calls = installFetch(
      shell([box({ status: "stopped" })], {
        "POST /v1/sandboxes/box-1/start": { json: box() },
      }),
    );
    await openBox();

    expect(screen.queryByRole("link", { name: /Terminal/ })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("command")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Start/ }));

    await waitFor(() => expect(countTo(calls, "POST", "/v1/sandboxes/box-1/start")).toBe(1));
  });

  it("forks one child and selects it", async () => {
    const child = box({ id: "box-child", parent: "box-1" });
    let live = [box()];
    const calls = installFetch(
      shell([], {
        "GET /v1/sandboxes": () => list(live),
        "POST /v1/sandboxes/box-1/fork": () => {
          live = [...live, child];
          return { json: { sandboxes: [child] } };
        },
      }),
    );
    await openBox();

    await userEvent.click(await screen.findByRole("button", { name: /Fork/ }));
    expect(await screen.findByText("box-child", { selector: "aside span" })).toBeInTheDocument();
    expect(calls.find((c) => c.url === "/v1/sandboxes/box-1/fork")!.body).toEqual({ count: 1 });
  });

  it("surfaces the stop reason and the warnings that came with it", async () => {
    installFetch(
      shell([
        box({
          status: "stopped",
          reason: "idle timeout",
          warnings: ["disk almost full", "pinned image"],
        }),
      ]),
    );
    await openBox();

    expect(await screen.findByText(/stopped: idle timeout/)).toHaveTextContent(
      "stopped: idle timeout · disk almost full · pinned image",
    );
  });

  it("closes the detail pane back to the bare list", async () => {
    installFetch(shell([box()]));
    await openBox();

    await userEvent.click(screen.getByRole("button", { name: "close" }));
    await waitFor(() => expect(detailOpen()).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: /New sandbox/ })).toBeInTheDocument();
  });
});

describe("Kill a sandbox", () => {
  it("asks first and only deletes once the dialog is confirmed", async () => {
    const calls = installFetch(shell([box()], { "DELETE /v1/sandboxes/box-1": { status: 204 } }));
    await openBox();

    await userEvent.click(await screen.findByRole("button", { name: /^Kill/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Kill box-1?")).toBeInTheDocument();
    expect(
      within(dialog).getByText("Its files are deleted. This can't be undone."),
    ).toBeInTheDocument();
    // Nothing has been deleted while the question is on screen.
    expect(countTo(calls, "DELETE", "/v1/sandboxes/box-1")).toBe(0);

    await userEvent.click(within(dialog).getByRole("button", { name: "Kill" }));
    await waitFor(() => expect(countTo(calls, "DELETE", "/v1/sandboxes/box-1")).toBe(1));
    await waitFor(() => expect(detailOpen()).not.toBeInTheDocument());
  });

  it("leaves the sandbox alone when the dialog is cancelled", async () => {
    const calls = installFetch(shell([box()], { "DELETE /v1/sandboxes/box-1": { status: 204 } }));
    await openBox();

    await userEvent.click(await screen.findByRole("button", { name: /^Kill/ }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", {
        name: "Cancel",
      }),
    );

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(countTo(calls, "DELETE", "/v1/sandboxes/box-1")).toBe(0);
    expect(detailOpen()).toBeInTheDocument();
  });
});

describe("File browser", () => {
  const files = (extra: Routes) =>
    shell([box()], {
      "POST /v1/sandboxes/box-1/exec": lsFor({
        ".": {
          json: {
            exit_code: 0,
            stdout: "src/\nmain.py\nnotes.txt\n",
            stderr: "",
          },
        },
      }),
      ...extra,
    });

  it("lists /workspace and reads a file through the files API", async () => {
    const calls = installFetch(
      files({
        "GET /v1/sandboxes/box-1/files": ({ url }) =>
          url.endsWith("path=main.py")
            ? { text: "print('hi')\n", contentType: "text/plain" }
            : apiError("no such file", undefined, 404),
      }),
    );
    await openBox();

    expect(await screen.findByRole("button", { name: "main.py" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "src/" })).toBeInTheDocument();
    expect(screen.getByText("/workspace")).toBeInTheDocument();
    // The listing is an `ls` exec, shell-quoted so odd names stay one word.
    expect(execCmds(calls, "box-1")).toEqual(["ls -1Ap -- '.'"]);

    await userEvent.click(screen.getByRole("button", { name: "main.py" }));
    expect(await screen.findByText("print('hi')")).toBeInTheDocument();
    expect(
      calls.filter((c) => c.url.startsWith("/v1/sandboxes/box-1/files?")).map((c) => c.url),
    ).toEqual(["/v1/sandboxes/box-1/files?path=main.py"]);
  });

  it("shows the server's message for a file it cannot read", async () => {
    installFetch(
      files({
        "GET /v1/sandboxes/box-1/files": apiError("no such file", "notes.txt was removed", 404),
      }),
    );
    await openBox();

    await userEvent.click(await screen.findByRole("button", { name: "notes.txt" }));
    // The read has no inline error slot, so the message arrives as a toast.
    expect(await screen.findByText("no such file (notes.txt was removed)")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "close file" })).not.toBeInTheDocument();
  });

  it("re-lists a subdirectory and offers a way back up", async () => {
    const calls = installFetch(
      files({
        "POST /v1/sandboxes/box-1/exec": lsFor({
          ".": { json: { exit_code: 0, stdout: "src/\n", stderr: "" } },
          src: { json: { exit_code: 0, stdout: "app.py\n", stderr: "" } },
        }),
        "GET /v1/sandboxes/box-1/files": {
          text: "",
          contentType: "text/plain",
        },
      }),
    );
    await openBox();

    await userEvent.click(await screen.findByRole("button", { name: "src/" }));
    expect(await screen.findByText("/workspace/src")).toBeInTheDocument();
    expect(execCmds(calls, "box-1")).toEqual(["ls -1Ap -- '.'", "ls -1Ap -- 'src'"]);

    await userEvent.click(screen.getByRole("button", { name: "../" }));
    expect(await screen.findByText("/workspace")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "src/" })).toBeInTheDocument();
  });

  it("drops the open file when the directory changes", async () => {
    installFetch(
      files({
        "POST /v1/sandboxes/box-1/exec": lsFor({
          ".": {
            json: { exit_code: 0, stdout: "src/\nmain.py\n", stderr: "" },
          },
          src: { json: { exit_code: 0, stdout: "", stderr: "" } },
        }),
        "GET /v1/sandboxes/box-1/files": {
          text: "body of main.py",
          contentType: "text/plain",
        },
      }),
    );
    await openBox();

    await userEvent.click(await screen.findByRole("button", { name: "main.py" }));
    expect(await screen.findByText("body of main.py")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "src/" }));
    await waitFor(() => expect(screen.queryByText("body of main.py")).not.toBeInTheDocument());
  });

  it("explains an empty workspace", async () => {
    installFetch(
      shell([box()], {
        "POST /v1/sandboxes/box-1/exec": {
          json: { exit_code: 0, stdout: "\n", stderr: "" },
        },
      }),
    );
    await openBox();

    expect(await screen.findByText("empty; drop files here to upload")).toBeInTheDocument();
  });

  it("surfaces the stderr of a listing that failed", async () => {
    installFetch(
      shell([box()], {
        "POST /v1/sandboxes/box-1/exec": {
          json: {
            exit_code: 1,
            stdout: "",
            stderr: "ls: cannot access '/workspace': Permission denied",
          },
        },
      }),
    );
    await openBox();

    expect(
      await screen.findByText("ls: cannot access '/workspace': Permission denied"),
    ).toBeInTheDocument();
  });

  it("uploads a chosen file as octet-stream and re-lists the directory", async () => {
    const calls = installFetch(files({ "PUT /v1/sandboxes/box-1/files": { status: 204 } }));
    await openBox();
    await screen.findByRole("button", { name: "main.py" });
    const before = execCmds(calls, "box-1").length;

    await userEvent.upload(
      document.querySelector<HTMLInputElement>('input[type="file"]')!,
      new File(["hello"], "greet.txt"),
    );

    await waitFor(() => expect(countTo(calls, "PUT", "/v1/sandboxes/box-1/files")).toBe(1));
    const put = calls.find((c) => c.method === "PUT")!;
    expect(put.url).toBe("/v1/sandboxes/box-1/files?path=greet.txt");
    expect(put.body).toBe("hello");
    // The upload invalidates the listing, so it is read again.
    await waitFor(() => expect(execCmds(calls, "box-1").length).toBeGreaterThan(before));
  });

  it("refuses an oversized file without sending it", async () => {
    const calls = installFetch(files({ "PUT /v1/sandboxes/box-1/files": { status: 204 } }));
    await openBox();
    await screen.findByRole("button", { name: "main.py" });

    const big = new File(["x"], "huge.bin");
    Object.defineProperty(big, "size", { value: 101 * 1024 * 1024 });
    await userEvent.upload(document.querySelector<HTMLInputElement>('input[type="file"]')!, big);

    expect(
      await screen.findByText("huge.bin: files over 100 MB can't be uploaded here"),
    ).toBeInTheDocument();
    expect(countTo(calls, "PUT", "/v1/sandboxes/box-1/files")).toBe(0);
  });
});
