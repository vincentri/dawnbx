// @vitest-environment jsdom
import { screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { installFetch, releaseFetch } from "@/test-fetch";
import { ME, renderApp, STATUS } from "@/test-support";

afterEach(releaseFetch);

/** The socket the page opens, with just enough surface to drive it by hand. */
class FakeSocket {
  static OPEN = 1;
  static opened: FakeSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: ArrayBuffer }) => void) | null = null;
  onclose: ((e: { code: number; reason: string }) => void) | null = null;
  binaryType = "";
  readyState = 0;
  sent: (string | ArrayBuffer)[] = [];
  closed = false;
  constructor(
    readonly url: string,
    readonly protocols: string[],
  ) {
    FakeSocket.opened.push(this);
  }
  open() {
    this.readyState = FakeSocket.OPEN;
    this.onopen?.();
  }
  send(d: string | ArrayBuffer) {
    this.sent.push(d);
  }
  close() {
    this.closed = true;
  }
  /** Deliver a binary frame the way the server's wsWriter would. */
  deliver(text: string) {
    this.onmessage?.({
      data: new TextEncoder().encode(text).buffer as ArrayBuffer,
    });
  }
}

const open = async (id = "box-1") => {
  FakeSocket.opened = [];
  vi.stubGlobal("WebSocket", FakeSocket);
  const calls = installFetch({
    "GET /v1/me": { json: ME },
    "GET /v1/status": { json: STATUS },
  });
  renderApp({ entry: `/terminal/${id}`, me: ME });
  await waitFor(() => expect(FakeSocket.opened).toHaveLength(1));
  return { calls, socket: FakeSocket.opened[0] };
};

/** What the xterm DOM renderer actually painted. */
const painted = () => document.querySelector(".xterm-rows")?.textContent ?? "";

it("upgrades /v1/sandboxes/{id}/terminal to a ws:// socket speaking the dawnbx subprotocol", async () => {
  const { socket } = await open("box-7f3a");

  expect(socket.url).toBe("ws://localhost:3000/v1/sandboxes/box-7f3a/terminal");
  expect(socket.protocols).toEqual(["dawnbx"]);
  expect(socket.binaryType).toBe("arraybuffer");
  // Still handshaking: nothing may be sent before the socket opens.
  expect(socket.sent).toEqual([]);
  expect(screen.getByText("box-7f3a")).toBeInTheDocument();
  expect(screen.getByText("connecting…")).toBeInTheDocument();
});

it("announces the connection and sends the terminal size once open", async () => {
  const { socket } = await open();

  socket.open();

  await waitFor(() => expect(screen.getByText("connected")).toBeInTheDocument());
  expect(socket.sent).toHaveLength(1);
  expect(JSON.parse(socket.sent[0] as string)).toEqual({
    cols: expect.any(Number),
    rows: expect.any(Number),
  });
});

it("renders the output frames the server sends", async () => {
  const { socket } = await open();
  socket.open();
  await waitFor(() => expect(screen.getByText("connected")).toBeInTheDocument());

  socket.deliver("root@box-1:/workspace# ");
  await waitFor(() => expect(painted()).toContain("root@box-1:/workspace#"));
  socket.deliver("ls\nmain.py\n");
  await waitFor(() => expect(painted()).toContain("main.py"));
});

it("reads a normal close as the session ending", async () => {
  const { socket } = await open();
  socket.open();
  await waitFor(() => expect(screen.getByText("connected")).toBeInTheDocument());

  socket.onclose?.({ code: 1000, reason: "shell exited" });

  await waitFor(() => expect(screen.getByText("session ended")).toBeInTheDocument());
});

it("reads an API close (4000+status) as the server's error message", async () => {
  const { socket } = await open();
  socket.open();
  await waitFor(() => expect(screen.getByText("connected")).toBeInTheDocument());

  socket.onclose?.({ code: 4404, reason: "not_found: no such sandbox" });

  await waitFor(() =>
    expect(screen.getByText("closed: not_found: no such sandbox")).toBeInTheDocument(),
  );
});

it("falls back to the close code when the server sends no reason", async () => {
  const { socket } = await open();
  socket.onclose?.({ code: 1011, reason: "" });

  await waitFor(() => expect(screen.getByText("closed: 1011")).toBeInTheDocument());
});

it("links back to the sandbox the terminal belongs to", async () => {
  await open("box-9");

  expect(screen.getByRole("link", { name: /Back/ })).toHaveAttribute("href", "/?id=box-9");
});
