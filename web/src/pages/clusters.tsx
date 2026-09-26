import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { cn } from "cn";
import { useRef, useState } from "react";
import { toast } from "sonner";
import { Confirm } from "@/components/confirm";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
import { age, api, must, when } from "@/lib/api";
import type { components } from "@/lib/schema";
import { useMe } from "./shell";

type Cluster = components["schemas"]["Cluster"];
type ClusterNode = components["schemas"]["ClusterNode"];
type Estimate = components["schemas"]["Estimate"];

// The wizard's place is the URL (?step) and so is the cluster it is about
// (?name): a provisioning page that survives a reload can be pasted to a
// colleague, which is the point of a control plane.
type Step = "provider" | "configure" | "price" | "status" | "nodes";
const STEPS: Step[] = ["provider", "configure", "price", "status", "nodes"];

type Config = {
  provider: string;
  name: string;
  region: string;
  instance_type: string;
  disk_gib: number;
  domain: string;
};

type Search = { step?: string; name?: string; provider?: string };

// The wizard's whole state is three query parameters, so both route trees
// validate them the same way and a hand-edited URL cannot smuggle a value in.
export const clusterSearch = (s: Record<string, unknown>): Search =>
  Object.fromEntries(
    ["step", "name", "provider"]
      .filter((k) => typeof s[k] === "string")
      .map((k) => [k, s[k] as string]),
  ) as Search;

const BLANK: Config = {
  provider: "",
  name: "",
  region: "",
  instance_type: "",
  disk_gib: 30,
  domain: "",
};

type Go = (next: { step: Step; name?: string; provider?: string }) => void;

const clusterPath = (id: string) => ({ params: { path: { name: id } } });

// openapi.yaml pins {provider} to the one phase-one provider, so this is
// where a second cloud lands: a new value here, never a new path family.
// The provider is a plain string in the generated types, because the contract
// deliberately does not pin it to one cloud: an unavailable provider answers 400
// provider_unavailable, and a type that rejected the value would make that
// unreachable from this client.
const providerPath = (id: string) => ({ params: { path: { provider: id } } });

// must() keeps the envelope's code on the thrown error; the codes below are
// the ones that change what the operator can do next.
const codeOf = (e: unknown) => (e as { code?: string } | null)?.code;
const textOf = (e: unknown) => (e instanceof Error ? e.message : String(e));

const usd = (n: number) => `$${n.toFixed(4)}`;
const month = (n: number) => `$${n.toFixed(2)}`;

const statusVariant = (s: string) =>
  s === "ready" ? "default" : s === "failed" ? "destructive" : "secondary";

export function Clusters() {
  const me = useMe().data!;
  const search = useSearch({ from: "/clusters" });
  const nav = useNavigate();
  const step: Step = STEPS.find((x) => x === search.step) ?? "provider";
  const go: Go = (next) => nav({ to: "/clusters", search: next });
  if (!me.admin)
    return (
      <p className="p-6 text-sm text-muted-foreground">
        Clusters are managed by control-plane administrators.
      </p>
    );
  return (
    <div className="mx-auto flex h-full max-w-6xl gap-6 overflow-auto p-6">
      <div className="min-w-0 flex-1">
        {(step === "provider" || step === "configure" || step === "price") && (
          <Wizard step={step} go={go} />
        )}
        {step === "status" &&
          (search.name ? <Status name={search.name} go={go} /> : <Pick go={go} />)}
        {step === "nodes" &&
          (search.name ? <Nodes name={search.name} go={go} /> : <Pick go={go} />)}
      </div>
      <aside className="w-72 shrink-0">
        <ClusterList />
      </aside>
    </div>
  );
}

// Stand-in for a ?name that is not there; the wizard is the only way in.
function Pick({ go }: { go: Go }) {
  return (
    <Section title="Clusters" desc="Choose a cluster on the right, or start a new one.">
      <Button onClick={() => go({ step: "provider" })}>New cluster</Button>
    </Section>
  );
}

// The three pre-creation steps. One component so the draft survives a step
// change: the operator goes back to fix the region without losing the name.
function Wizard({ step, go }: { step: Step; go: Go }) {
  const qc = useQueryClient();
  const { provider: inUrl } = useSearch({ from: "/clusters" });
  const [draft, setDraft] = useState<Config>({ ...BLANK, provider: inUrl ?? "" });
  const [quoted, setQuoted] = useState<{ quote: Estimate; config: Config }>();
  const set = (k: keyof Config, v: string | number) => setDraft((d) => ({ ...d, [k]: v }));

  const providers = useQuery({
    queryKey: ["providers"],
    queryFn: async () => (await must(api.GET("/v1/providers"))).providers,
  });
  const regions = useQuery({
    queryKey: ["regions", draft.provider],
    queryFn: async () =>
      (await must(api.GET("/v1/providers/{provider}/regions", providerPath(draft.provider))))
        .regions,
    enabled: !!draft.provider,
  });
  const types = useInstanceTypes(draft.provider);

  const estimate = useMutation({
    mutationFn: () =>
      must(
        api.POST("/v1/providers/{provider}/estimate", {
          ...providerPath(draft.provider),
          body: {
            region: draft.region,
            instance_type: draft.instance_type,
            disk_gib: draft.disk_gib,
            ...(draft.domain.trim() && { domain: draft.domain.trim() }),
          },
        }),
      ),
    onSuccess: (quote) => {
      setQuoted({ quote, config: { ...draft } });
      go({ step: "price", provider: draft.provider });
    },
  });
  const create = useMutation({
    mutationFn: () =>
      must(
        api.POST("/v1/clusters", {
          body: {
            name: draft.name.trim(),
            region: draft.region,
            instance_type: draft.instance_type,
            disk_gib: draft.disk_gib,
            ...(draft.domain.trim() && { domain: draft.domain.trim() }),
            quote_id: quoted!.quote.quote_id,
          },
        }),
      ),
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ["clusters"] });
      go({ step: "status", name: c.name });
    },
  });

  if (step === "provider")
    return (
      <Section
        title="Provider"
        desc="AWS provisions in phase one. The others are listed so the choice is never a guess."
      >
        <div className="flex flex-wrap gap-2">
          {(providers.data ?? []).map((p) => (
            <Button
              key={p.id}
              variant={p.available ? "default" : "outline"}
              // An unavailable provider stays visible and refuses to be picked:
              // there is no partial GCP or Azure flow to fall into (FR-002).
              disabled={!p.available}
              onClick={() => {
                set("provider", p.id);
                go({ step: "configure", provider: p.id });
              }}
            >
              {p.id}
              {!p.available && <Badge variant="outline">unavailable</Badge>}
            </Button>
          ))}
        </div>
        {/* The delivery guarantee, before a provider is chosen: it is part of
            what the choice is, and after the choice the page has navigated on. */}
        <DeliveryNote id={draft.provider} providers={providers.data} />
        {providers.data?.length === 0 && (
          <p className="text-sm text-muted-foreground">The server reports no provider.</p>
        )}
        {providers.error && <Fail e={providers.error} />}
      </Section>
    );

  if (step === "configure")
    return (
      <Section
        title="Configuration"
        desc="Nothing is created and nothing is charged until you have seen the price."
      >
        <Field label="Cluster name" hint="lowercase letters, digits and dashes">
          <Input
            className="w-56"
            aria-label="cluster name"
            value={draft.name}
            onChange={(e) => set("name", e.target.value)}
          />
        </Field>
        <Field label="Region">
          <Picker
            label="region"
            value={draft.region}
            onChange={(v) => set("region", v)}
            options={regions.data ?? []}
            empty="No region to choose from."
          />
        </Field>
        {regions.error && <Fail e={regions.error} />}
        <Field label="Size">
          <Picker
            label="size"
            value={draft.instance_type}
            onChange={(v) => set("instance_type", v)}
            options={(types.data?.instance_types ?? []).map((t) => t.id)}
            empty="No size to choose from."
            price={(id) => {
              const t = (types.data?.instance_types ?? []).find((x) => x.id === id);
              return t ? `${usd(t.hourly_usd)}/h · ${month(t.monthly_usd)}/mo` : "";
            }}
          />
        </Field>
        {types.error && <Fail e={types.error} />}
        {types.data && <Catalogue region={draft.region} pricedFor={types.data.region} />}
        <Field label="Disk (GiB)">
          <Input
            className="w-28"
            type="number"
            min={8}
            aria-label="disk"
            value={draft.disk_gib}
            onChange={(e) => set("disk_gib", Number(e.target.value))}
          />
        </Field>
        <Field label="Domain" hint="optional; a name for the cluster's URL">
          <Input
            className="w-56"
            aria-label="domain"
            value={draft.domain}
            onChange={(e) => set("domain", e.target.value)}
          />
        </Field>
        <div className="flex items-center gap-2">
          <Button
            disabled={
              !draft.name.trim() || !draft.region || !draft.instance_type || estimate.isPending
            }
            onClick={() => estimate.mutate()}
          >
            {estimate.isPending ? "Estimating…" : "Estimate price"}
          </Button>
          <Button
            variant="ghost"
            onClick={() => go({ step: "provider", provider: draft.provider })}
          >
            Back
          </Button>
        </div>
        {!draft.name.trim() || !draft.region || !draft.instance_type ? (
          <p className="text-sm text-muted-foreground">
            Name the cluster and pick a region and a size to see a price.
          </p>
        ) : null}
        {estimate.error && <Fail e={estimate.error} />}
      </Section>
    );

  if (!quoted)
    return (
      <Section title="Price" desc="No estimate yet, so there is nothing to confirm.">
        <Button onClick={() => go({ step: "configure", provider: draft.provider })}>
          Configure the cluster
        </Button>
      </Section>
    );
  // The quote is the gate on spending money: without one there is nothing to
  // confirm, and the estimate names the configuration it prices.
  const q = quoted.quote;
  return (
    <Section
      title="Price"
      desc="Fixed charges for this configuration. Nothing is created until you confirm."
    >
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Charge</TableHead>
            <TableHead className="text-right">Hourly</TableHead>
            <TableHead className="text-right">Monthly</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {q.lines.map((l) => (
            <TableRow key={l.label}>
              <TableCell>{l.label}</TableCell>
              <TableCell className="text-right font-mono">{usd(l.hourly_usd)}</TableCell>
              <TableCell className="text-right font-mono">{month(l.monthly_usd)}</TableCell>
            </TableRow>
          ))}
          <TableRow>
            <TableCell className="font-medium">Total</TableCell>
            <TableCell className="text-right font-mono font-medium">{usd(q.hourly_usd)}</TableCell>
            <TableCell className="text-right font-mono font-medium">
              {month(q.monthly_usd)}
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
      <p className="text-sm text-muted-foreground">
        Not estimated: {q.excluded.join(", ")}. Those move with your use, and the provider's
        discounts can cut this further.
      </p>
      <p className="text-sm text-muted-foreground">
        Priced for {quoted.config.instance_type} · {quoted.config.region} · {quoted.config.disk_gib}{" "}
        GiB
        {quoted.config.domain ? ` · ${quoted.config.domain}` : ""}
      </p>
      {codeOf(create.error) === "quote_stale" && (
        <p className="text-sm text-destructive">
          The price changed before the cluster was created. Review the new estimate and confirm
          again. {textOf(create.error)}
        </p>
      )}
      {create.error && codeOf(create.error) !== "quote_stale" && <Fail e={create.error} />}
      <div className="flex items-center gap-2">
        <Confirm
          title={`Create ${draft.name.trim()} on ${draft.provider}?`}
          body={`${month(q.monthly_usd)} a month at this configuration. It starts billing as soon as it is created.`}
          action="Create cluster"
          onConfirm={() => create.mutate()}
        >
          <Button disabled={create.isPending}>
            {create.isPending ? "Creating…" : "Confirm and create"}
          </Button>
        </Confirm>
        <Button variant="ghost" onClick={() => go({ step: "configure", provider: draft.provider })}>
          Change configuration
        </Button>
      </div>
    </Section>
  );
}

// The host sizes a provider offers, and the region it priced them for. Both come
// from one response: the region is a fact about the whole answer, which is why
// the contract declares it beside the list rather than inside each entry.
function useInstanceTypes(provider: string) {
  return useQuery({
    queryKey: ["instance-types", provider],
    queryFn: async () =>
      await must(api.GET("/v1/providers/{provider}/instance-types", providerPath(provider))),
    enabled: !!provider,
  });
}

// DeliveryNote names how the chosen provider puts a credential on a new host.
// The guarantee genuinely differs by cloud — one with no secret store an
// instance can read cannot keep the value out of the creation payload — so an
// operator choosing a provider is told which one they are getting.
function DeliveryNote({
  id,
  providers,
}: {
  id: string;
  providers: { id: string; delivery?: string }[] | undefined;
}) {
  const line = providers?.find((p) => p.id === (id || providers[0]?.id));
  if (!line?.delivery) return null;
  return (
    <p className="text-sm text-muted-foreground">
      {line.id} delivers the administrator password to a new host through {line.delivery}. Other
      clouds differ, and this is the guarantee you get here.
    </p>
  );
}

function Catalogue({ region, pricedFor }: { region: string; pricedFor: string }) {
  // The response says which region it priced, so a mismatch is said out loud
  // rather than left for the operator to assume.
  if (!pricedFor || pricedFor === region) return null;
  return (
    <p className="text-sm text-muted-foreground">
      These prices are the provider's catalogue for {pricedFor}, not {region}. The estimate is what
      you are charged against.
    </p>
  );
}

function Field(props: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-3">
      <Label className="w-28 shrink-0">{props.label}</Label>
      {props.children}
      {props.hint && <span className="text-sm text-muted-foreground">{props.hint}</span>}
    </div>
  );
}

function Picker(props: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: string[];
  empty: string;
  price?: (id: string) => string;
}) {
  return (
    <Select value={props.value} onValueChange={props.onChange}>
      <SelectTrigger className="w-72" aria-label={props.label}>
        <SelectValue placeholder={props.empty} />
      </SelectTrigger>
      <SelectContent>
        {props.options.map((o) => (
          <SelectItem key={o} value={o}>
            {o}
            {props.price?.(o) ? ` · ${props.price(o)}` : ""}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

// isInFlight is the states worth waiting on: a cluster that has not finished
// being made, and one that has not finished being removed. Anything else is an
// answer, and a poll would only be refreshing a value that will not change.
function isInFlight(status: string | undefined) {
  return status === "provisioning" || status === "deleting";
}

function Status({ name: id, go }: { name: string; go: Go }) {
  const qc = useQueryClient();
  const cluster = useQuery({
    queryKey: ["cluster", id],
    queryFn: () => must(api.GET("/v1/clusters/{name}", clusterPath(id))),
    // SC-003 allows 60s; a 5s poll lands a phase change well inside it and
    // stops as soon as the outcome is known. `deleting` is still in flight —
    // the teardown is running — so it is watched too, or the screen would sit on
    // "deleting" for ever.
    refetchInterval: (q) => (isInFlight(q.state.data?.status) ? 5000 : false),
  });
  // Whether this operator deleted the cluster. A 404 without that fact is a
  // cluster that vanished on its own, which reads very differently.
  const deleted = useRef(false);
  const destroy = useMutation({
    mutationFn: () => must(api.DELETE("/v1/clusters/{name}", clusterPath(id))),
    onSuccess: () => {
      deleted.current = true;
      qc.invalidateQueries({ queryKey: ["clusters"] });
      qc.invalidateQueries({ queryKey: ["cluster", id] });
    },
  });
  const c = cluster.data;
  return (
    <div className="flex flex-col gap-4">
      <Section title={id} desc="Created without an SSH session; the control plane holds it.">
        {/* After a delete the record is gone, so this refetch 404s. That is the
            end of the flow this operator started, not a failure to report. */}
        {cluster.error && deleted.current ? (
          <p className="text-sm text-muted-foreground">
            This cluster is gone. Its resources were removed; the record went with them.
          </p>
        ) : (
          cluster.error && <Fail e={cluster.error} />
        )}
        {!c && !cluster.error && <p className="text-sm text-muted-foreground">Loading…</p>}
        {c && (
          <>
            <div className="flex items-center gap-2">
              <Badge variant={statusVariant(c.status)}>{c.status}</Badge>
              <span className="text-sm text-muted-foreground">
                {c.provider} · {c.region} · {c.instance_type} · {c.disk_gib} GiB
                {c.domain ? ` · ${c.domain}` : ""}
              </span>
              <span className="text-xs text-muted-foreground">updated {age(c.updated)} ago</span>
            </div>
            <Timeline c={c} />
            <Failure c={c} />
            {/* The detail is also the only place a cluster that is stuck but not
                failed says why. Without it a wedged cluster renders as a calm
                "provisioning: verifying" with no reason and no action. */}
            {c.status !== "failed" && c.detail && (
              <p className="text-sm text-muted-foreground">
                {c.detail}
                {isInFlight(c.status) && " — still in flight, but this is why."}
              </p>
            )}
            {c.url && (
              <a
                className="font-mono underline underline-offset-2"
                href={c.url}
                target="_blank"
                rel="noreferrer"
              >
                {c.url}
              </a>
            )}
            {c.tls_pin && (
              <div>
                <p className="text-sm text-muted-foreground">
                  Certificate pin (SHA-256 SPKI) — compare it out of band before you trust this
                  cluster.
                </p>
                <CopyValue value={c.tls_pin} small />
              </div>
            )}
            <p className="text-sm text-muted-foreground">
              {usd(c.hourly_usd)}/h · {month(c.monthly_usd)}/mo · created {when(c.created)}
            </p>
            <div className="flex flex-wrap items-center gap-2">
              <Button variant="outline" onClick={() => go({ step: "nodes", name: id })}>
                Worker nodes
              </Button>
              {c.status !== "deleted" && (
                <Confirm
                  title={`Delete ${id}?`}
                  body="Its workers, sandboxes and the host itself are destroyed. This cannot be undone."
                  action="Delete cluster"
                  onConfirm={() => destroy.mutate()}
                >
                  <Button variant="destructive" disabled={destroy.isPending}>
                    {destroy.isPending ? "Deleting…" : "Delete cluster"}
                  </Button>
                </Confirm>
              )}
            </div>
            {destroy.error && <Fail e={destroy.error} />}
            <Credentials name={id} />
          </>
        )}
      </Section>
    </div>
  );
}

// The spine is the lifecycle the contract defines. The server's own
// sub-step is shown beside the stage rather than guessed at here, because
// the phase names are the adapter's, not this page's.
const SPINE = ["provisioning", "ready"] as const;

function Timeline({ c }: { c: Cluster }) {
  const at = c.status === "provisioning" ? 0 : 1;
  const off = !SPINE.includes(c.status as (typeof SPINE)[number]);
  return (
    <ol className="flex flex-wrap items-center gap-2 text-xs">
      {SPINE.map((s, i) => (
        <li
          key={s}
          className={cn(
            "rounded border px-2 py-0.5",
            i <= at ? "border-primary" : "border-border text-muted-foreground",
            i === at && "font-medium",
          )}
        >
          {s}
          {i === at && c.phase ? `: ${c.phase}` : ""}
        </li>
      ))}
      {off && (
        <li className="rounded border border-destructive px-2 py-0.5 font-medium text-destructive">
          {c.status}
        </li>
      )}
    </ol>
  );
}

// A failure has to name its phase, its reason and what to do next, or the
// operator goes looking for an SSH session (SC-004).
function Failure({ c }: { c: Cluster }) {
  if (c.status !== "failed") return null;
  return (
    <div className="rounded-md border border-destructive/50 bg-destructive/5 p-3 text-sm">
      <p className="font-medium">Provisioning failed{c.phase ? ` in ${c.phase}` : ""}.</p>
      <p>{c.detail || "The provider reported no reason."}</p>
      <p className="mt-1 text-muted-foreground">
        Next step: read the reason above, then delete this cluster and configure another. Anything
        paid for by this attempt is removed automatically; this diagnostic is what is kept. No
        secret value is in it.
      </p>
      <p className="text-muted-foreground">
        SSH is for rescue work — a wedged host, a full disk, a broken upgrade. It is not part of
        this flow.
      </p>
    </div>
  );
}

function Credentials({ name: id }: { name: string }) {
  const qc = useQueryClient();
  const [revealed, setRevealed] = useState(false);
  const creds = useQuery({
    queryKey: ["cluster-credentials", id],
    queryFn: () => must(api.GET("/v1/clusters/{name}/credentials", clusterPath(id))),
    enabled: revealed,
  });
  const rotate = useMutation({
    mutationFn: () => must(api.POST("/v1/clusters/{name}/rotate", clusterPath(id))),
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ["cluster", id] });
      void qc.invalidateQueries({ queryKey: ["cluster-credentials", id] });
      qc.setQueryData(["cluster-credentials", id], undefined);
      setRevealed(true);
      if (c.detail) toast.info(c.detail);
    },
  });
  return (
    <div className="flex flex-col gap-2 border-t pt-3">
      <div className="flex items-center gap-2">
        <p className="text-sm font-medium">Credentials</p>
        <Button size="sm" variant="outline" disabled={revealed} onClick={() => setRevealed(true)}>
          {revealed ? "Shown" : "Reveal"}
        </Button>
        <Confirm
          title="Rotate the cluster credentials?"
          body="Mints a new admin password and API key, and revokes the cluster's previous API key straight away. The old password keeps working until you apply the new pair, so nothing is locked out in between."
          action="Rotate"
          onConfirm={() => rotate.mutate()}
        >
          <Button size="sm" variant="destructive" disabled={rotate.isPending}>
            Rotate
          </Button>
        </Confirm>
      </div>
      <p className="text-sm text-muted-foreground">
        Reading these is recorded in the audit log as cluster.credentials.view. This replaces
        reading them off the host.
      </p>
      {codeOf(creds.error) === "credentials_not_ready" && (
        <p className="text-sm text-muted-foreground">
          The cluster mints its API key while it verifies, so there is nothing to reveal yet. This
          panel fills in on its own once the cluster is ready.
        </p>
      )}
      {creds.error && codeOf(creds.error) !== "credentials_not_ready" && <Fail e={creds.error} />}
      {creds.data && (
        <div className="flex flex-col gap-2">
          <div>
            <p className="text-xs text-muted-foreground">API key</p>
            <CopyValue value={creds.data.api_key} small />
          </div>
          <div>
            <p className="text-xs text-muted-foreground">Admin password</p>
            <CopyValue value={creds.data.admin_password} small />
          </div>
        </div>
      )}
      {rotate.error && <Fail e={rotate.error} />}
    </div>
  );
}

function Nodes({ name: id, go }: { name: string; go: Go }) {
  const qc = useQueryClient();
  const cluster = useQuery({
    queryKey: ["cluster", id],
    queryFn: () => must(api.GET("/v1/clusters/{name}", clusterPath(id))),
  });
  const provider = cluster.data?.provider ?? "";
  const types = useInstanceTypes(provider);
  const [instance, setInstance] = useState("");
  const [disk, setDisk] = useState(30);
  const nodes = useQuery({
    queryKey: ["cluster-nodes", id],
    queryFn: async () => (await must(api.GET("/v1/clusters/{name}/nodes", clusterPath(id)))).nodes,
    // Poll only while something is moving; a settled worker list is static.
    refetchInterval: (q) =>
      (q.state.data ?? []).some((n) => n.status === "provisioning") ? 5000 : false,
  });
  const refresh = () => qc.invalidateQueries({ queryKey: ["cluster-nodes", id] });
  const add = useMutation({
    mutationFn: () =>
      must(
        api.POST("/v1/clusters/{name}/nodes", {
          ...clusterPath(id),
          body: { instance_type: instance, disk_gib: disk },
        }),
      ),
    onSuccess: () => {
      setInstance("");
      refresh();
    },
  });
  const remove = useMutation({
    mutationFn: (node: string) =>
      must(
        api.DELETE("/v1/clusters/{name}/nodes/{node}", { params: { path: { name: id, node } } }),
      ),
    onSuccess: refresh,
  });
  return (
    <Section
      title="Worker nodes"
      desc="Workers host sandboxes. A worker that holds sandboxes cannot be removed until they are gone."
    >
      <div className="flex flex-wrap items-center gap-2">
        <Select value={instance} onValueChange={setInstance}>
          <SelectTrigger className="w-64" aria-label="worker size">
            <SelectValue placeholder="Worker size" />
          </SelectTrigger>
          <SelectContent>
            {(types.data?.instance_types ?? []).map((t) => (
              <SelectItem key={t.id} value={t.id}>
                {t.id} · {usd(t.hourly_usd)}/h
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="w-24"
          type="number"
          min={8}
          aria-label="worker disk"
          value={disk}
          onChange={(e) => setDisk(Number(e.target.value))}
        />
        <Button disabled={!instance || add.isPending} onClick={() => add.mutate()}>
          {add.isPending ? "Adding…" : "Add worker"}
        </Button>
        <Button variant="ghost" onClick={() => go({ step: "status", name: id })}>
          Back to the cluster
        </Button>
      </div>
      {codeOf(add.error) === "cluster_unavailable" && (
        <p className="text-sm text-destructive">
          The cluster is not ready to take a worker. Wait for it to report ready, then add one.{" "}
          {textOf(add.error)}
        </p>
      )}
      {add.error && codeOf(add.error) !== "cluster_unavailable" && <Fail e={add.error} />}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Node</TableHead>
            <TableHead>Size</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Sandboxes</TableHead>
            <TableHead>Added</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {(nodes.data ?? []).map((n) => (
            <NodeRow
              key={n.id}
              n={n}
              onRemove={() => remove.mutate(n.id)}
              removing={remove.isPending}
            />
          ))}
        </TableBody>
      </Table>
      {nodes.data?.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No workers yet; everything runs on the cluster host.
        </p>
      )}
      {nodes.error && <Fail e={nodes.error} />}
      {codeOf(remove.error) === "node_holds_sandboxes" && (
        <p className="text-sm text-destructive">
          Still holding sandboxes, so the removal was refused. {textOf(remove.error)}
        </p>
      )}
      {remove.error && codeOf(remove.error) !== "node_holds_sandboxes" && <Fail e={remove.error} />}
    </Section>
  );
}

function NodeRow(props: { n: ClusterNode; onRemove: () => void; removing: boolean }) {
  const n = props.n;
  // The cluster is the authority on what a worker holds, so this is a
  // courtesy; the removal itself is still refused with the count (FR-011).
  const busy = n.sandboxes > 0;
  return (
    <TableRow>
      <TableCell className="font-mono">{n.id}</TableCell>
      <TableCell>{n.instance_type}</TableCell>
      <TableCell>
        <Badge variant={statusVariant(n.status)}>{n.status}</Badge>
        {n.detail && <span className="ml-2 text-xs text-muted-foreground">{n.detail}</span>}
      </TableCell>
      <TableCell>{n.sandboxes}</TableCell>
      <TableCell>{when(n.created)}</TableCell>
      <TableCell>
        {busy ? (
          <span className="text-xs text-muted-foreground">
            holds {n.sandboxes} sandbox{n.sandboxes === 1 ? "" : "es"}
          </span>
        ) : (
          <Confirm
            title={`Remove worker ${n.id}?`}
            body="Its sandboxes, if any, go with it. This cannot be undone."
            action="Remove"
            onConfirm={props.onRemove}
          >
            <Button size="xs" variant="destructive" disabled={props.removing}>
              Remove
            </Button>
          </Confirm>
        )}
      </TableCell>
    </TableRow>
  );
}

function ClusterList() {
  const clusters = useQuery({
    queryKey: ["clusters"],
    queryFn: async () => (await must(api.GET("/v1/clusters"))).clusters,
    refetchInterval: (q) => ((q.state.data ?? []).some((c) => isInFlight(c.status)) ? 5000 : false),
  });
  const nav = useNavigate();
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Clusters</CardTitle>
        <CardDescription>Every cluster this control plane manages.</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-2">
        {clusters.error && <Fail e={clusters.error} />}
        {(clusters.data ?? []).map((c) => (
          <button
            key={c.name}
            type="button"
            className="flex items-center justify-between gap-2 rounded border px-2 py-1 text-left text-sm hover:bg-muted"
            onClick={() => nav({ to: "/clusters", search: { step: "status", name: c.name } })}
          >
            <span className="truncate font-mono">{c.name}</span>
            <Badge variant={statusVariant(c.status)}>{c.status}</Badge>
          </button>
        ))}
        {clusters.data?.length === 0 && (
          <p className="text-sm text-muted-foreground">No clusters yet.</p>
        )}
      </CardContent>
    </Card>
  );
}

function Section(props: { title: string; desc?: string; children: React.ReactNode }) {
  return (
    <Card>
      <CardHeader>
        <h2 className="font-heading text-base leading-snug font-medium">{props.title}</h2>
        {props.desc && <CardDescription>{props.desc}</CardDescription>}
      </CardHeader>
      <CardContent className="grid gap-4">{props.children}</CardContent>
    </Card>
  );
}

// One place to copy a secret or a pin.
function CopyValue({ value, small }: { value: string; small?: boolean }) {
  return (
    <div className="flex gap-2">
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

// Every page error is shown where it happened, not only toasted: a silent
// empty table on a provisioning screen reads as "nothing is wrong".
function Fail({ e }: { e: unknown }) {
  return <p className="text-sm text-destructive">{textOf(e)}</p>;
}
