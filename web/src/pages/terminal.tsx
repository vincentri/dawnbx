import { Link, useParams } from "@tanstack/react-router";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import { useEffect, useRef, useState } from "react";
import "@xterm/xterm/css/xterm.css";
import { ArrowLeftIcon } from "lucide-react";
import { Button } from "@/components/ui/button";

// TerminalPage is a full-screen shell over /v1/sandboxes/{id}/terminal.
// The session cookie rides along on the upgrade; the server checks Origin.
export function TerminalPage() {
  const { id } = useParams({ from: "/terminal/$id" });
  const box = useRef<HTMLDivElement>(null);
  const [state, setState] = useState("connecting…");
  useEffect(() => {
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: "ui-monospace, Menlo, monospace",
      fontSize: 13,
      theme: { background: "#0a0a0a" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(box.current!);
    fit.fit();
    const ws = new WebSocket(
      `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/sandboxes/${id}/terminal`,
      ["dawnbx"],
    );
    ws.binaryType = "arraybuffer";
    const enc = new TextEncoder();
    const send = (d: string | Uint8Array) => ws.readyState === WebSocket.OPEN && ws.send(d);
    const size = () => send(JSON.stringify({ cols: term.cols, rows: term.rows }));
    let done = false;
    ws.onopen = () => {
      setState("connected");
      size();
      term.focus();
    };
    ws.onmessage = (e) => term.write(new Uint8Array(e.data));
    // API errors close with 4000+status and the message as reason.
    ws.onclose = (e) =>
      !done && setState(e.code === 1000 ? "session ended" : `closed: ${e.reason || e.code}`);
    const d1 = term.onData((d) => send(enc.encode(d)));
    const d2 = term.onResize(size);
    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(box.current!);
    return () => {
      done = true;
      ro.disconnect();
      d1.dispose();
      d2.dispose();
      ws.close();
      term.dispose();
    };
  }, [id]);
  return (
    <div className="flex h-full flex-col bg-[#0a0a0a]">
      <div className="flex items-center gap-3 border-b px-3 py-1.5 text-sm">
        <Button variant="ghost" size="sm" asChild>
          <Link to="/" search={{ id }}>
            <ArrowLeftIcon /> Back
          </Link>
        </Button>
        <span className="font-mono">{id}</span>
        <span className="text-muted-foreground">{state}</span>
      </div>
      <div ref={box} className="min-h-0 flex-1 p-2" />
    </div>
  );
}
