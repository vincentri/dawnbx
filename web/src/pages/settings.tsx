import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { cn } from "cn";
import { useState } from "react";
import { toast } from "sonner";
import { Confirm } from "@/components/confirm";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, must, type Principal, when } from "@/lib/api";
import { signOut, useMe } from "./shell";

export function Settings() {
  const me = useMe().data!;
  const { tab = "keys" } = useSearch({ from: "/settings" });
  const nav = useNavigate();
  return (
    <div className="mx-auto h-full max-w-5xl overflow-auto p-6">
      <Tabs value={tab} onValueChange={(t) => nav({ to: "/settings", search: { tab: t } })}>
        <TabsList>
          <TabsTrigger value="keys">API keys</TabsTrigger>
          <TabsTrigger value="audit">Audit log</TabsTrigger>
          {me.admin && <TabsTrigger value="users">Users</TabsTrigger>}
          {me.admin && <TabsTrigger value="orgs">Orgs</TabsTrigger>}
          {me.admin && <TabsTrigger value="nodes">Nodes</TabsTrigger>}
          <TabsTrigger value="account">Account</TabsTrigger>
        </TabsList>
        <TabsContent value="keys">
          <Keys me={me} />
        </TabsContent>
        <TabsContent value="audit">
          <Audit />
        </TabsContent>
        {me.admin && (
          <>
            <TabsContent value="users">
              <Users me={me} />
            </TabsContent>
            <TabsContent value="orgs">
              <Orgs />
            </TabsContent>
            <TabsContent value="nodes">
              <Nodes />
            </TabsContent>
          </>
        )}
        <TabsContent value="account">
          <Account />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function Section(props: { title: string; desc?: string; children: React.ReactNode }) {
  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>{props.title}</CardTitle>
        {props.desc && <CardDescription>{props.desc}</CardDescription>}
      </CardHeader>
      <CardContent className="grid gap-4">{props.children}</CardContent>
    </Card>
  );
}

// One place to copy a secret or command; `small` for the long join command.
function CopyValue({ value, small }: { value: string; small?: boolean }) {
  return (
    <div key="cmd" className="flex gap-2">
      <code
        className={cn(
          "flex-1 rounded bg-muted px-2 py-1 font-mono break-all select-all",
          small && "text-xs",
        )}
      >
        {value}
      </code>
      <Button
        size="sm"
        variant="outline"
        onClick={() => navigator.clipboard.writeText(value).then(() => toast.success("Copied"))}
      >
        Copy
      </Button>
    </div>
  );
}

function Grid(props: { head: string[]; rows: React.ReactNode[][]; empty?: string }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          {props.head.map((h, i) => (
            <TableHead key={i}>{h}</TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {props.rows.map((r, i) => (
          <TableRow key={i}>
            {r.map((c, j) => (
              <TableCell key={j}>{c}</TableCell>
            ))}
          </TableRow>
        ))}
        {!props.rows.length && props.empty && (
          <TableRow>
            <TableCell colSpan={props.head.length} className="text-center text-muted-foreground">
              {props.empty}
            </TableCell>
          </TableRow>
        )}
      </TableBody>
    </Table>
  );
}

function OrgSelect(props: { value: string; onChange: (v: string) => void }) {
  const orgs = useQuery({
    queryKey: ["orgs"],
    queryFn: async () => (await must(api.GET("/v1/orgs"))).orgs,
  });
  return (
    <Select value={props.value} onValueChange={props.onChange}>
      <SelectTrigger className="w-36" aria-label="org">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {(orgs.data ?? [{ id: props.value }]).map((o) => (
          <SelectItem key={o.id} value={o.id}>
            {o.id}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function Keys({ me }: { me: Principal }) {
  const qc = useQueryClient();
  const keys = useQuery({
    queryKey: ["keys"],
    queryFn: async () => (await must(api.GET("/v1/keys"))).keys,
  });
  const [name, setName] = useState("");
  const [ttl, setTtl] = useState("never");
  const [org, setOrg] = useState(me.org);
  const [token, setToken] = useState<string>();
  const create = useMutation({
    mutationFn: () =>
      must(
        api.POST("/v1/keys", {
          body: { name, ...(ttl !== "never" && { ttl }), ...(me.admin && { org }) },
        }),
      ),
    onSuccess: (r) => {
      setToken(r.key);
      setName("");
      qc.invalidateQueries({ queryKey: ["keys"] });
    },
  });
  const revoke = useMutation({
    mutationFn: (id: string) =>
      must(api.DELETE("/v1/keys/{key}", { params: { path: { key: id } } })),
    onSuccess: () => {
      setToken(undefined);
      qc.invalidateQueries({ queryKey: ["keys"] });
    },
  });
  return (
    <Section
      title="API keys"
      desc="SDKs and the CLI use these (DAWNBX_API_KEY). The token is shown once."
    >
      <form
        className="flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Input
          className="w-56"
          placeholder="name, e.g. ci"
          required
          maxLength={100}
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        {me.admin && <OrgSelect value={org} onChange={setOrg} />}
        <Select value={ttl} onValueChange={setTtl}>
          <SelectTrigger className="w-40" aria-label="expires">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="never">never expires</SelectItem>
            <SelectItem value="720h">30 days</SelectItem>
            <SelectItem value="2160h">90 days</SelectItem>
            <SelectItem value="8760h">1 year</SelectItem>
          </SelectContent>
        </Select>
        <Button type="submit" disabled={create.isPending}>
          Create key
        </Button>
      </form>
      {token && (
        <div className="rounded-md border border-primary/50 bg-primary/5 p-3 text-sm">
          <p className="mb-2">Copy this now; it won't be shown again.</p>
          <CopyValue value={token} />
        </div>
      )}
      <Grid
        head={["ID", "Name", "Org", "Created", "Last used", "Expires", ""]}
        empty="No keys yet."
        rows={[...(keys.data ?? [])].reverse().map((k) => [
          <span key="id" className="font-mono">
            {k.id}
          </span>,
          k.name,
          k.org,
          when(k.created),
          when(k.last_used),
          k.expires ? when(k.expires) : "never",
          k.revoked ? (
            <Badge key="revoked" variant="outline">
              revoked
            </Badge>
          ) : (
            <Confirm
              key="revoke"
              title={`Revoke "${k.name}"?`}
              body="Clients using it stop working within 30 seconds."
              action="Revoke"
              onConfirm={() => revoke.mutate(k.id)}
            >
              <Button size="xs" variant="destructive">
                Revoke
              </Button>
            </Confirm>
          ),
        ])}
      />
    </Section>
  );
}

function Audit() {
  const ev = useQuery({
    queryKey: ["audit"],
    queryFn: async () => (await must(api.GET("/v1/audit"))).events,
  });
  return (
    <Section title="Audit log" desc="Latest 200 events.">
      <Grid
        head={["When", "Org", "Actor", "Action", "Target"]}
        empty="Nothing yet."
        rows={(ev.data ?? []).map((e) => [
          when(e.at),
          e.org,
          e.actor,
          e.action,
          <span key="target" className="font-mono">
            {e.target}
          </span>,
        ])}
      />
    </Section>
  );
}

function Users({ me }: { me: Principal }) {
  const qc = useQueryClient();
  const users = useQuery({
    queryKey: ["users"],
    queryFn: async () => (await must(api.GET("/v1/users"))).users,
  });
  const [f, setF] = useState({
    username: "",
    password: "",
    org: me.org,
    role: "member" as "member" | "admin",
  });
  const done = () => qc.invalidateQueries({ queryKey: ["users"] });
  const create = useMutation({
    mutationFn: () => must(api.POST("/v1/users", { body: { ...f, username: f.username.trim() } })),
    onSuccess: () => {
      setF({ ...f, username: "", password: "" });
      done();
    },
  });
  const del = useMutation({
    mutationFn: (u: string) =>
      must(api.DELETE("/v1/users/{username}", { params: { path: { username: u } } })),
    onSuccess: done,
  });
  return (
    <Section title="Users" desc="Dashboard sign-ins. Admins see and manage every org.">
      <form
        className="flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Input
          className="w-40"
          placeholder="username"
          required
          maxLength={64}
          autoComplete="off"
          value={f.username}
          onChange={(e) => setF({ ...f, username: e.target.value })}
        />
        <Input
          className="w-52"
          type="password"
          placeholder="password (10+ chars)"
          required
          minLength={10}
          maxLength={72}
          autoComplete="new-password"
          value={f.password}
          onChange={(e) => setF({ ...f, password: e.target.value })}
        />
        <OrgSelect value={f.org} onChange={(org) => setF({ ...f, org })} />
        <Select
          value={f.role}
          onValueChange={(role) => setF({ ...f, role: role as typeof f.role })}
        >
          <SelectTrigger className="w-28" aria-label="role">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="member">member</SelectItem>
            <SelectItem value="admin">admin</SelectItem>
          </SelectContent>
        </Select>
        <Button type="submit" disabled={create.isPending}>
          Add user
        </Button>
      </form>
      <Grid
        head={["Username", "Org", "Role", "Created", ""]}
        rows={(users.data ?? []).map((u) => [
          u.username,
          u.org,
          u.role,
          when(u.created),
          u.username === me.user ? (
            <span key="who" className="text-muted-foreground">
              you
            </span>
          ) : (
            <div key="actions" className="flex gap-2">
              <ResetPassword username={u.username} />
              <Confirm
                title={`Delete user ${u.username}?`}
                body="Their sessions end now. Their API keys keep working."
                action="Delete"
                onConfirm={() => del.mutate(u.username)}
              >
                <Button size="xs" variant="destructive">
                  Delete
                </Button>
              </Confirm>
            </div>
          ),
        ])}
      />
    </Section>
  );
}

function ResetPassword({ username }: { username: string }) {
  const [open, setOpen] = useState(false);
  const [pw, setPw] = useState("");
  const reset = useMutation({
    mutationFn: () =>
      must(
        api.POST("/v1/users/{username}/password", {
          params: { path: { username } },
          body: { password: pw },
        }),
      ),
    onSuccess: () => {
      setOpen(false);
      setPw("");
      toast.success(`Password for ${username} changed`);
    },
  });
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="xs" variant="outline">
          Reset password
        </Button>
      </DialogTrigger>
      <DialogContent>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            reset.mutate();
          }}
        >
          <DialogHeader>
            <DialogTitle>New password for {username}</DialogTitle>
          </DialogHeader>
          <Input
            type="password"
            placeholder="10 to 72 chars"
            required
            minLength={10}
            maxLength={72}
            autoComplete="new-password"
            value={pw}
            onChange={(e) => setPw(e.target.value)}
          />
          <DialogFooter>
            <Button type="submit" disabled={reset.isPending}>
              Set password
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function Orgs() {
  const qc = useQueryClient();
  const orgs = useQuery({
    queryKey: ["orgs"],
    queryFn: async () => (await must(api.GET("/v1/orgs"))).orgs,
  });
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => must(api.POST("/v1/orgs", { body: { id: id.trim(), name } })),
    onSuccess: () => {
      setId("");
      setName("");
      qc.invalidateQueries({ queryKey: ["orgs"] });
    },
  });
  return (
    <Section title="Orgs" desc="Members see only their org's sandboxes and keys.">
      <form
        className="flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Input
          className="w-40"
          placeholder="id, e.g. acme"
          required
          pattern="[a-z0-9][a-z0-9\-]{0,39}"
          value={id}
          onChange={(e) => setId(e.target.value)}
        />
        <Input
          className="w-56"
          placeholder="display name (optional)"
          maxLength={100}
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <Button type="submit" disabled={create.isPending}>
          Add org
        </Button>
      </form>
      <Grid
        head={["ID", "Name", "Created"]}
        rows={(orgs.data ?? []).map((o) => [
          <span key="id" className="font-mono">
            {o.id}
          </span>,
          o.name,
          o.created.startsWith("1970") ? "–" : when(o.created),
        ])}
      />
    </Section>
  );
}

function Nodes() {
  const qc = useQueryClient();
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: async () => (await must(api.GET("/v1/nodes"))).nodes,
    refetchInterval: 5000,
  });
  const join = useMutation({ mutationFn: () => must(api.GET("/v1/nodes/join")) });
  const remove = useMutation({
    mutationFn: (name: string) =>
      must(api.DELETE("/v1/nodes/{name}", { params: { path: { name } } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["nodes"] }),
  });
  const workers = nodes.data?.some((n) => n.role === "worker");
  return (
    <Section
      title="Nodes"
      desc="Workers add room for sandboxes. Run the join command as root on a fresh Linux box."
    >
      <div className="flex flex-col gap-2">
        <Button
          variant="outline"
          className="w-fit"
          disabled={join.isPending}
          onClick={() => join.mutate()}
        >
          Show join command
        </Button>
        {join.data && <CopyValue value={join.data.command} small />}
      </div>
      {nodes.data && !workers && (
        <p className="text-sm text-muted-foreground">
          No workers yet; everything runs on this server.
        </p>
      )}
      <Grid
        head={["Name", "Role", "Status", "IP", "Sandboxes", "Kubelet", ""]}
        rows={(nodes.data ?? []).map((n) => [
          n.name,
          n.role,
          n.ready ? (
            <Badge key="ready">Ready</Badge>
          ) : (
            <Badge key="notready" variant="destructive">
              NotReady since {when(n.since)}
            </Badge>
          ),
          n.ip,
          n.sandboxes,
          n.kubelet,
          n.role === "worker" && (
            <Confirm
              title={`Remove node ${n.name}?`}
              body="Also run k3s-agent-uninstall.sh on it, or it rejoins."
              action="Remove"
              onConfirm={() => remove.mutate(n.name)}
            >
              <Button size="xs" variant="destructive">
                Remove
              </Button>
            </Confirm>
          ),
        ])}
      />
    </Section>
  );
}

function Account() {
  const qc = useQueryClient();
  const [old, setOld] = useState("");
  const [pw, setPw] = useState("");
  const change = useMutation({
    mutationFn: () => must(api.POST("/v1/me/password", { body: { old, new: pw } })),
    onSuccess: () => {
      // The server ends every session of this user.
      signOut(qc);
      toast.success("Password changed. Sign in with the new one.");
    },
  });
  return (
    <Section title="Change password" desc="Signs you out everywhere.">
      <form
        className="flex max-w-sm flex-col gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          change.mutate();
        }}
      >
        <Input
          type="password"
          placeholder="current password"
          required
          autoComplete="current-password"
          value={old}
          onChange={(e) => setOld(e.target.value)}
        />
        <Input
          type="password"
          placeholder="new password (10+ chars)"
          required
          minLength={10}
          maxLength={72}
          autoComplete="new-password"
          value={pw}
          onChange={(e) => setPw(e.target.value)}
        />
        <Button type="submit" className="w-fit" disabled={change.isPending}>
          Change password
        </Button>
      </form>
    </Section>
  );
}
