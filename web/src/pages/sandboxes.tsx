import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { CopyIcon, FileIcon, FolderIcon, PlayIcon, PlusIcon, SkullIcon, TerminalIcon, TimerIcon, UploadIcon, XIcon } from "lucide-react";
import { toast } from "sonner";
import { Confirm } from "@/components/confirm";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { age, api, left, must, q, when, type Sandbox } from "@/lib/api";

const sid = (id: string) => ({ params: { path: { id } } });

function useSandboxes() {
  return useQuery({
    queryKey: ["sandboxes"],
    queryFn: async () => (await must(api.GET("/v1/sandboxes"))).sandboxes,
    refetchInterval: 2000,
  });
}

export function Sandboxes() {
  const { id } = useSearch({ from: "/" });
  const list = useSandboxes();
  const sel = list.data?.find((s) => s.id === id);
  return (
    <div className="flex h-full">
      <section className="flex min-w-0 flex-1 flex-col">
        <CreateForm />
        <div className="min-h-0 flex-1 overflow-auto">
          <SandboxTable list={list.data ?? []} sel={id} />
          {list.error && <p className="p-4 text-sm text-destructive">{list.error.message}</p>}
          {list.data?.length === 0 && <p className="p-8 text-center text-sm text-muted-foreground">No sandboxes yet. Create one above.</p>}
        </div>
      </section>
      {sel && <Detail key={sel.id} s={sel} list={list.data ?? []} />}
    </div>
  );
}

function CreateForm() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [image, setImage] = useState("");
  const [ttl, setTtl] = useState("");
  const [network, setNetwork] = useState<"internet" | "none">("internet");
  const create = useMutation({
    mutationFn: () => must(api.POST("/v1/sandboxes", { body: { network, ...(image.trim() && { image: image.trim() }), ...(ttl.trim() && { ttl: ttl.trim() === "forever" ? null : ttl.trim() }) } })),
    onSuccess: (s) => {
      qc.invalidateQueries({ queryKey: ["sandboxes"] });
      nav({ to: "/", search: { id: s.id } });
    },
  });
  return (
    <form
      className="flex flex-wrap items-center gap-2 border-b p-3"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate();
      }}
    >
      <Input className="w-56" placeholder="python:3.12-slim" aria-label="image" value={image} onChange={(e) => setImage(e.target.value)} />
      <Select value={network} onValueChange={(v) => setNetwork(v as typeof network)}>
        <SelectTrigger className="w-32" aria-label="network">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="internet">internet</SelectItem>
          <SelectItem value="none">no network</SelectItem>
        </SelectContent>
      </Select>
      <Input className="w-24" placeholder="ttl 1h" title="e.g. 30m, 2h, or forever" aria-label="ttl" value={ttl} onChange={(e) => setTtl(e.target.value)} />
      <Button type="submit" disabled={create.isPending}>
        <PlusIcon /> {create.isPending ? "Creating…" : "New sandbox"}
      </Button>
    </form>
  );
}

const statusVariant = (s: string) => (s === "running" ? "default" : s === "stopped" ? "secondary" : "outline");

function SandboxTable({ list, sel }: { list: Sandbox[]; sel?: string }) {
  const nav = useNavigate();
  const multiNode = new Set(list.map((s) => s.node)).size > 1;
  if (!list.length) return null;
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>ID</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Image</TableHead>
          <TableHead>Network</TableHead>
          <TableHead>Parent</TableHead>
          <TableHead>Age</TableHead>
          <TableHead>Expires</TableHead>
          {multiNode && <TableHead>Node</TableHead>}
        </TableRow>
      </TableHeader>
      <TableBody>
        {list.map((s) => (
          <TableRow
            key={s.id}
            data-state={s.id === sel ? "selected" : undefined}
            className="cursor-pointer"
            onClick={() => nav({ to: "/", search: { id: s.id } })}
          >
            <TableCell className="font-mono">{s.id}</TableCell>
            <TableCell>
              <Badge variant={statusVariant(s.status)}>{s.status}</Badge>
            </TableCell>
            <TableCell>{s.image}</TableCell>
            <TableCell>{s.network}</TableCell>
            <TableCell className="font-mono">{s.parent && <SbLink id={s.parent} />}</TableCell>
            <TableCell>{age(s.created)}</TableCell>
            <TableCell>{left(s.expires_at)}</TableCell>
            {multiNode && <TableCell>{s.node}</TableCell>}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function SbLink({ id }: { id: string }) {
  return (
    <Link to="/" search={{ id }} className="font-mono underline underline-offset-2" onClick={(e) => e.stopPropagation()}>
      {id}
    </Link>
  );
}

function Detail({ s, list }: { s: Sandbox; list: Sandbox[] }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const refresh = () => qc.invalidateQueries({ queryKey: ["sandboxes"] });
  const useAct = <T,>(fn: () => Promise<T>) => useMutation({ mutationFn: fn, onSuccess: refresh });
  const extend = useAct(() => must(api.POST("/v1/sandboxes/{id}/extend", { ...sid(s.id), body: { ttl: "1h" } })));
  const forever = useAct(() => must(api.POST("/v1/sandboxes/{id}/extend", { ...sid(s.id), body: { ttl: null } })));
  const start = useAct(() => must(api.POST("/v1/sandboxes/{id}/start", sid(s.id))));
  const kill = useAct(() => must(api.DELETE("/v1/sandboxes/{id}", sid(s.id))));
  const fork = useMutation({
    mutationFn: () => must(api.POST("/v1/sandboxes/{id}/fork", { ...sid(s.id), body: { count: 1 } })),
    onSuccess: (r) => {
      refresh();
      nav({ to: "/", search: { id: r.sandboxes[0].id } });
    },
  });
  const running = s.status === "running";
  const kids = list.filter((k) => k.parent === s.id);
  const info: [string, React.ReactNode][] = [
    ["image", s.image],
    ["network", s.network],
    ["created", when(s.created)],
    ["expires", s.expires_at ? `${when(s.expires_at)} (in ${left(s.expires_at)})` : "never"],
    ["node", s.node],
    ["org", s.org],
    ["restarted", s.restarted_at && when(s.restarted_at)],
    ["parent", s.parent && <SbLink id={s.parent} />],
    ["children", kids.length > 0 && <span className="flex flex-wrap gap-2">{kids.map((k) => <SbLink key={k.id} id={k.id} />)}</span>],
  ];
  const notes = [s.reason && `stopped: ${s.reason}`, ...(s.warnings ?? [])].filter(Boolean);
  return (
    <aside className="flex w-[34rem] shrink-0 flex-col overflow-auto border-l">
      <div className="flex items-center gap-2 border-b p-3">
        <span className="font-mono font-semibold">{s.id}</span>
        <Badge variant={statusVariant(s.status)}>{s.status}</Badge>
        <Button variant="ghost" size="icon-sm" className="ml-auto" aria-label="close" onClick={() => nav({ to: "/", search: {} })}>
          <XIcon />
        </Button>
      </div>
      {notes.length > 0 && <p className="border-b bg-destructive/10 px-3 py-2 text-sm text-destructive">{notes.join(" · ")}</p>}
      <dl className="grid grid-cols-[6rem_1fr] gap-x-3 gap-y-1 p-3 text-sm">
        {info
          .filter(([, v]) => v)
          .map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="break-all">{v}</dd>
            </div>
          ))}
      </dl>
      <div className="flex flex-wrap gap-2 border-b px-3 pb-3">
        {running && (
          <>
            <Button size="sm" asChild>
              <Link to="/terminal/$id" params={{ id: s.id }}>
                <TerminalIcon /> Terminal
              </Link>
            </Button>
            <Button size="sm" variant="outline" disabled={fork.isPending} onClick={() => fork.mutate()}>
              <CopyIcon /> {fork.isPending ? "Forking…" : "Fork"}
            </Button>
          </>
        )}
        {s.status === "stopped" && (
          <Button size="sm" disabled={start.isPending} onClick={() => start.mutate()}>
            <PlayIcon /> Start
          </Button>
        )}
        <Button size="sm" variant="outline" disabled={extend.isPending} onClick={() => extend.mutate()}>
          <TimerIcon /> Extend 1h
        </Button>
        {s.expires_at && (
          <Button size="sm" variant="outline" disabled={forever.isPending} onClick={() => forever.mutate()}>
            Keep forever
          </Button>
        )}
        <Confirm
          title={`Kill ${s.id}?`}
          body="Its files are deleted. This can't be undone."
          action="Kill"
          onConfirm={() => kill.mutate(undefined, { onSuccess: () => nav({ to: "/", search: {} }) })}
        >
          <Button size="sm" variant="destructive" className="ml-auto" disabled={kill.isPending}>
            <SkullIcon /> Kill
          </Button>
        </Confirm>
      </div>
      {running && <Run id={s.id} />}
      {running && <Files id={s.id} />}
    </aside>
  );
}

function Run({ id }: { id: string }) {
  const qc = useQueryClient();
  const [cmd, setCmd] = useState("");
  const [out, setOut] = useState<{ cmd: string; stdout: string; stderr: string; code: number; ms: number }>();
  const run = useMutation({
    mutationFn: async (c: string) => {
      const t = Date.now();
      const r = await must(api.POST("/v1/sandboxes/{id}/exec", { ...sid(id), body: { cmd: c } }));
      return { cmd: c, stdout: r.stdout, stderr: r.stderr, code: r.exit_code, ms: Date.now() - t };
    },
    onSuccess: (r) => {
      setOut(r);
      qc.invalidateQueries({ queryKey: ["ls", id] });
    },
  });
  return (
    <div className="border-b p-3">
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (cmd.trim()) run.mutate(cmd);
        }}
      >
        <Input className="font-mono" placeholder="ls -la" autoComplete="off" aria-label="command" value={cmd} onChange={(e) => setCmd(e.target.value)} />
        <Button type="submit" size="sm" className="h-8" disabled={run.isPending}>
          {run.isPending ? "Running…" : "Run"}
        </Button>
      </form>
      {out && (
        <pre className="mt-2 max-h-80 overflow-auto rounded-md bg-muted p-2 font-mono text-xs whitespace-pre-wrap">
          <span className="text-muted-foreground">$ {out.cmd}{"\n"}</span>
          {out.stdout}
          <span className="text-destructive">{out.stderr}</span>
          <span className="text-muted-foreground">
            {"\n"}exit {out.code} · {out.ms} ms
          </span>
        </pre>
      )}
    </div>
  );
}

// Files lists /workspace via exec (ls) and reads/writes through the files API.
function Files({ id }: { id: string }) {
  const [cwd, setCwd] = useState(".");
  const [file, setFile] = useState<{ path: string; text: string }>();
  const [drop, setDrop] = useState(false);
  const up = useRef<HTMLInputElement>(null);
  const qc = useQueryClient();
  const ls = useQuery({
    queryKey: ["ls", id, cwd],
    queryFn: () => must(api.POST("/v1/sandboxes/{id}/exec", { ...sid(id), body: { cmd: `ls -1Ap -- ${q(cwd)}` } })),
  });
  useEffect(() => setFile(undefined), [cwd]);
  const join = (name: string) => (cwd === "." ? name : `${cwd}/${name}`);
  const cat = useMutation({
    mutationFn: async (path: string) => {
      const r = await must(api.GET("/v1/sandboxes/{id}/files", { params: { path: { id }, query: { path } }, parseAs: "text" }));
      const text = r as unknown as string;
      return { path, text: text.length > 200000 ? text.slice(0, 200000) + "\n… (truncated)" : text };
    },
    onSuccess: setFile,
  });
  const upload = useMutation({
    mutationFn: async (files: File[]) => {
      for (const f of files) {
        if (f.size > 100 << 20) {
          toast.error(`${f.name}: files over 100 MB can't be uploaded here`);
          continue;
        }
        await must(
          api.PUT("/v1/sandboxes/{id}/files", {
            params: { path: { id }, query: { path: join(f.name) } },
            body: f as unknown as string,
            bodySerializer: (b) => b as unknown as BodyInit,
            headers: { "Content-Type": "application/octet-stream" },
          }),
        );
      }
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["ls", id] }),
  });
  const names = ls.data?.stdout.split("\n").filter(Boolean) ?? [];
  const entries: [string, () => void][] = [];
  if (cwd !== ".") entries.push(["../", () => setCwd(cwd.split("/").slice(0, -1).join("/") || ".")]);
  for (const n of names) {
    const path = join(n.replace(/\/$/, ""));
    entries.push([n, n.endsWith("/") ? () => setCwd(path) : () => cat.mutate(path)]);
  }
  return (
    <div className="flex min-h-0 flex-1 flex-col p-3">
      <div className="mb-2 flex items-center gap-2 text-sm">
        <span className="truncate font-mono text-muted-foreground">
          {upload.isPending ? "uploading…" : cwd === "." ? "/workspace" : `/workspace/${cwd}`}
        </span>
        <Button size="sm" variant="outline" className="ml-auto" onClick={() => up.current?.click()}>
          <UploadIcon /> Upload
        </Button>
        <input
          ref={up}
          type="file"
          multiple
          hidden
          onChange={(e) => {
            upload.mutate([...(e.target.files ?? [])]);
            e.target.value = "";
          }}
        />
      </div>
      <ul
        className={`min-h-24 rounded-md border p-1 font-mono text-sm ${drop ? "border-primary bg-primary/10" : ""}`}
        onDragOver={(e) => {
          e.preventDefault();
          setDrop(true);
        }}
        onDragLeave={() => setDrop(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDrop(false);
          upload.mutate([...e.dataTransfer.files]);
        }}
      >
        {entries.map(([n, fn]) => (
          <li key={n}>
            <button className="flex w-full items-center gap-2 rounded px-2 py-0.5 text-left hover:bg-muted" onClick={fn}>
              {n.endsWith("/") ? <FolderIcon className="size-3.5" /> : <FileIcon className="size-3.5" />}
              {n}
            </button>
          </li>
        ))}
        {ls.data && !entries.length && (
          <li className="px-2 py-1 text-muted-foreground">{ls.data.exit_code ? ls.data.stderr : "empty; drop files here to upload"}</li>
        )}
        {ls.error && <li className="px-2 py-1 text-destructive">{ls.error.message}</li>}
      </ul>
      {file && (
        <div className="mt-3">
          <div className="mb-1 flex items-center text-sm">
            <span className="font-mono">{file.path}</span>
            <Button variant="ghost" size="icon-sm" className="ml-auto" aria-label="close file" onClick={() => setFile(undefined)}>
              <XIcon />
            </Button>
          </div>
          <pre className="max-h-96 overflow-auto rounded-md bg-muted p-2 font-mono text-xs whitespace-pre-wrap">{file.text}</pre>
        </div>
      )}
    </div>
  );
}
