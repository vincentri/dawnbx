// dawnbx dashboard. Plain DOM; server data is only ever set via textContent.
const $ = (s) => document.querySelector(s);
const el = (tag, text, cls) => {
  const e = document.createElement(tag);
  if (text != null) e.textContent = text;
  if (cls) e.className = cls;
  return e;
};
const q = (s) => `'${s.replace(/'/g, `'\\''`)}'`; // single-quote for sh -c

// The session lives in an HttpOnly cookie, so page scripts never see a secret.
let me = null;
let list = [];
let sel = null; // selected sandbox id
let cwd = ".";

async function api(method, path, body, text) {
  const headers = { "X-Dawnbx": "1" }; // the server wants it on cookie-authed writes (CSRF)
  const raw = body instanceof Blob; // file uploads go as-is
  if (body !== undefined && !raw) headers["Content-Type"] = "application/json";
  const r = await fetch(path, { method, headers, body: body === undefined || raw ? body : JSON.stringify(body) });
  if (r.status === 401 && path !== "/v1/login") {
    signedOut();
    throw new Error("signed out (session expired); sign in again");
  }
  if (!r.ok) {
    const e = await r.json().catch(() => ({ message: `${r.status} ${r.statusText}` }));
    throw new Error(e.message + (e.hint ? ` (${e.hint})` : ""));
  }
  if (r.status === 204) return null;
  return text ? r.text() : r.json();
}

function show(err) {
  $("#msg").textContent = err instanceof Error ? err.message : String(err);
  $("#msg").hidden = false;
}
$("#msg").onclick = () => ($("#msg").hidden = true);

// Runs fn with the button disabled; errors go to the banner.
async function busy(btn, fn) {
  if (btn) btn.disabled = true;
  try {
    return await fn();
  } catch (e) {
    show(e);
  } finally {
    if (btn) btn.disabled = false;
  }
}

function dur(ms) {
  const s = Math.round(Math.abs(ms) / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d`;
}
const age = (t) => dur(Date.now() - Date.parse(t));
const left = (t) => (t ? dur(Date.parse(t) - Date.now()) : "never");

// --- auth ---
function signedOut() {
  me = null;
  clearInterval(timer);
  $("#app").hidden = $("#keys").hidden = true;
  $("#login").hidden = false;
}
$("#logout").onclick = async () => {
  await api("POST", "/v1/logout").catch(() => {});
  signedOut();
};
$("#login").onsubmit = async (e) => {
  e.preventDefault();
  await busy(e.submitter, async () => {
    me = await api("POST", "/v1/login", { username: $("#user").value.trim(), password: $("#pass").value });
    $("#pass").value = "";
    $("#msg").hidden = true;
    start();
  });
};

// --- settings: keys (everyone signed in), users + orgs (admins), password, audit ---
const when = (t) => (t ? new Date(t).toLocaleString() : "–");
function rowButton(label, ask, fn) {
  const b = el("button", label, "danger");
  b.onclick = () => busy(b, async () => {
    if (confirm(ask)) await fn();
  });
  return b;
}
$("#keys-btn").onclick = () => {
  $("#keys").hidden = false;
  $("#k-made").hidden = true;
  $("#k-who").textContent = `${me.user} · ${me.admin ? "admin (all orgs)" : `member of ${me.org}`}`;
  for (const e of document.querySelectorAll("#keys .admin")) e.hidden = !me.admin;
  loadSettings().catch(show);
};
$("#k-close").onclick = () => ($("#keys").hidden = true);

async function loadSettings() {
  const [{ keys }, { events }] = await Promise.all([api("GET", "/v1/keys"), api("GET", "/v1/audit")]);
  $("#k-rows").replaceChildren(
    ...keys.reverse().map((k) => {
      const tr = el("tr", null, k.revoked ? "revoked" : "");
      const td = el("td");
      td.append(k.revoked ? el("span", "revoked", "dim") : rowButton("Revoke", `Revoke key "${k.name}"? Anything using it stops working within 30 s.`, async () => {
        await api("DELETE", `/v1/keys/${k.id}`);
        $("#k-made").hidden = true;
        await loadSettings();
      }));
      tr.append(el("td", k.id), el("td", k.name), el("td", k.org), el("td", when(k.created)), el("td", when(k.last_used)), el("td", k.expires ? when(k.expires) : "never"), td);
      return tr;
    }),
  );
  $("#a-rows").replaceChildren(
    ...events.map((e) => {
      const tr = el("tr");
      tr.append(el("td", when(e.at)), el("td", e.org), el("td", e.actor), el("td", e.action), el("td", e.target));
      return tr;
    }),
  );
  if (!me.admin) return;
  const [{ users }, { orgs }] = await Promise.all([api("GET", "/v1/users"), api("GET", "/v1/orgs")]);
  for (const sel of ["#k-org", "#u-org"]) {
    const cur = $(sel).value || me.org;
    $(sel).replaceChildren(...orgs.map((o) => el("option", o.id)));
    $(sel).value = cur;
  }
  $("#u-rows").replaceChildren(
    ...users.map((u) => {
      const tr = el("tr");
      const td = el("td");
      if (u.username !== me.user) {
        const reset = el("button", "Set password");
        reset.onclick = () => busy(reset, async () => {
          const pw = prompt(`New password for ${u.username} (10+ chars). Their sessions end.`);
          if (pw) await api("POST", `/v1/users/${encodeURIComponent(u.username)}/password`, { password: pw });
        });
        td.append(reset, " ", rowButton("Delete", `Delete user ${u.username}? Keys they made keep working.`, async () => {
          await api("DELETE", `/v1/users/${encodeURIComponent(u.username)}`);
          await loadSettings();
        }));
      }
      tr.append(el("td", u.username), el("td", u.org), el("td", u.role), el("td", when(u.created)), td);
      return tr;
    }),
  );
  $("#o-rows").replaceChildren(
    ...orgs.map((o) => {
      const tr = el("tr");
      tr.append(el("td", o.id), el("td", o.name), el("td", o.created.startsWith("1970") ? "–" : when(o.created)));
      return tr;
    }),
  );
}

const form = (id, fn) => ($(id).onsubmit = (e) => {
  e.preventDefault();
  busy(e.submitter, async () => {
    await fn();
    e.target.reset();
    await loadSettings();
  });
});
form("#k-new", async () => {
  const body = { name: $("#k-name").value, ttl: $("#k-ttl").value };
  if (me.admin) body.org = $("#k-org").value;
  const r = await api("POST", "/v1/keys", body);
  $("#k-token").textContent = r.key;
  $("#k-made").hidden = false;
});
form("#u-new", () => api("POST", "/v1/users", { username: $("#u-name").value.trim(), password: $("#u-pass").value, org: $("#u-org").value, role: $("#u-role").value }));
form("#o-new", () => api("POST", "/v1/orgs", { id: $("#o-id").value.trim(), name: $("#o-name").value }));
$("#p-new").onsubmit = (e) => {
  e.preventDefault();
  busy(e.submitter, async () => {
    await api("POST", "/v1/me/password", { old: $("#p-old").value, new: $("#p-new1").value });
    e.target.reset();
    signedOut();
    show("Password changed. Sign in with the new one.");
  });
};

// --- list ---
async function refresh() {
  if (!me || document.hidden) return;
  try {
    const [st, l] = await Promise.all([api("GET", "/v1/status"), api("GET", "/v1/sandboxes")]);
    $("#stat").textContent = `${st.version} · ${Math.round(st.free_pct)}% disk free · warm ${st.warm}/${st.pool_size}`;
    list = l.sandboxes;
    renderRows();
    renderDetail();
  } catch (e) {
    show(e);
  }
}

function link(id) {
  const a = el("a", id);
  a.onclick = (e) => {
    e.stopPropagation();
    select(id);
  };
  return a;
}

function renderRows() {
  const rows = list.map((s) => {
    const tr = el("tr");
    if (s.id === sel) tr.className = "sel";
    tr.onclick = () => select(s.id);
    tr.append(
      el("td", s.id),
      el("td", s.status, s.status),
      el("td", s.image),
      el("td", s.network),
      s.parent ? el("td") : el("td", "–", "dim"),
      el("td", age(s.created)),
      el("td", left(s.expires_at)),
    );
    if (s.parent) tr.children[4].append(link(s.parent));
    return tr;
  });
  $("#rows").replaceChildren(...rows);
  $("#empty").hidden = list.length > 0;
}

// --- detail ---
function select(id) {
  if (sel !== id) {
    sel = id;
    cwd = ".";
    $("#out").hidden = $("#file").hidden = true;
    $("#files").replaceChildren();
  }
  renderRows();
  renderDetail();
  if (current()?.status === "running") ls();
}
const current = () => list.find((s) => s.id === sel);

function renderDetail() {
  const s = current();
  $("#detail").hidden = !s;
  if (!s) return;
  $("#d-id").textContent = s.id;
  $("#d-status").textContent = s.status;
  $("#d-status").className = `badge ${s.status}`;
  $("#d-note").textContent = [s.reason && `stopped: ${s.reason}`, ...(s.warnings || [])].filter(Boolean).join(" · ");
  const kids = list.filter((k) => k.parent === s.id);
  const info = [
    ["image", s.image],
    ["network", s.network],
    ["created", new Date(s.created).toLocaleString()],
    ["expires", s.expires_at ? `${new Date(s.expires_at).toLocaleString()} (in ${left(s.expires_at)})` : "never"],
    ["restarted", s.restarted_at && new Date(s.restarted_at).toLocaleString()],
    ["parent", s.parent && link(s.parent)],
    ["children", kids.length && kids.flatMap((k, i) => (i ? [" ", link(k.id)] : [link(k.id)]))],
  ].filter(([, v]) => v);
  $("#d-info").replaceChildren(
    ...info.flatMap(([k, v]) => {
      const dd = el("dd");
      dd.append(...[v].flat());
      return [el("dt", k), dd];
    }),
  );
  const running = s.status === "running";
  $("#a-start").hidden = s.status !== "stopped";
  for (const b of ["#a-term", "#a-fork", "#run button", "#cmd"]) $(b).disabled = !running;
}

const act = (btn, fn) => ($(btn).onclick = (e) => busy(e.currentTarget, async () => {
  await fn(sel);
  await refresh();
}));
act("#a-extend", (id) => api("POST", `/v1/sandboxes/${id}/extend`, { ttl: "1h" }));
act("#a-forever", (id) => api("POST", `/v1/sandboxes/${id}/extend`, { ttl: null }));
act("#a-start", (id) => api("POST", `/v1/sandboxes/${id}/start`));
act("#a-fork", async (id) => {
  const r = await api("POST", `/v1/sandboxes/${id}/fork`, { count: 1 });
  await refresh();
  select(r.sandboxes[0].id);
});
act("#a-kill", async (id) => {
  if (!confirm(`Kill ${id}? Its files are deleted.`)) return;
  await api("DELETE", `/v1/sandboxes/${id}`);
  sel = null;
});

$("#new").onsubmit = (e) => {
  e.preventDefault();
  const body = { network: $("#new-net").value };
  if ($("#new-image").value.trim()) body.image = $("#new-image").value.trim();
  if ($("#new-ttl").value.trim()) body.ttl = $("#new-ttl").value.trim();
  busy(e.submitter, async () => {
    const s = await api("POST", "/v1/sandboxes", body);
    await refresh();
    select(s.id);
  });
};

$("#run").onsubmit = (e) => {
  e.preventDefault();
  const cmd = $("#cmd").value;
  if (!cmd.trim()) return;
  const id = sel;
  const t = Date.now();
  busy(e.submitter, async () => {
    const r = await api("POST", `/v1/sandboxes/${id}/exec`, { cmd });
    if (id !== sel) return;
    const out = $("#out");
    out.replaceChildren(el("span", `$ ${cmd}\n`, "meta"), r.stdout, el("span", r.stderr, "err"),
      el("span", `\nexit ${r.exit_code} · ${Date.now() - t} ms`, "meta"));
    out.hidden = false;
    ls();
  });
};

// --- files (listed via exec, read via the files API) ---
async function ls() {
  const id = sel;
  const r = await api("POST", `/v1/sandboxes/${id}/exec`, { cmd: `ls -1Ap -- ${q(cwd)}` }).catch(show);
  if (!r || id !== sel) return;
  $("#cwd").textContent = cwd === "." ? "/workspace" : `/workspace/${cwd}`;
  const items = [];
  if (cwd !== ".") items.push(["../", () => cd(cwd.split("/").slice(0, -1).join("/") || ".")]);
  for (const name of r.stdout.split("\n").filter(Boolean)) {
    const path = cwd === "." ? name.replace(/\/$/, "") : `${cwd}/${name.replace(/\/$/, "")}`;
    items.push([name, name.endsWith("/") ? () => cd(path) : () => cat(path)]);
  }
  $("#files").replaceChildren(
    ...items.map(([name, fn]) => {
      const li = el("li");
      const a = el("a", name);
      a.onclick = fn;
      li.append(a);
      return li;
    }),
  );
  if (!items.length) $("#files").append(el("li", r.exit_code ? r.stderr : "(empty)", "dim"));
}
function cd(path) {
  cwd = path;
  $("#file").hidden = true;
  ls();
}
async function cat(path) {
  const id = sel;
  const text = await api("GET", `/v1/sandboxes/${id}/files?path=${encodeURIComponent(path)}`, undefined, true).catch(show);
  if (text == null || id !== sel) return;
  $("#file").replaceChildren(el("span", `${path}\n`, "meta"), text.length > 200000 ? text.slice(0, 200000) + "\n… truncated" : text);
  $("#file").hidden = false;
}

// Upload into the current folder, via the button or by dropping files on the list.
async function upload(picked) {
  const id = sel;
  for (const f of picked) {
    if (f.size > 100 << 20) {
      show(`${f.name}: files over 100 MB can't be uploaded here`);
      continue;
    }
    const path = cwd === "." ? f.name : `${cwd}/${f.name}`;
    $("#cwd").textContent = `uploading ${f.name}…`;
    await api("PUT", `/v1/sandboxes/${id}/files?path=${encodeURIComponent(path)}`, f).catch(show);
  }
  if (id === sel) ls();
}
$("#up-btn").onclick = () => $("#up").click();
$("#up").onchange = (e) => {
  upload([...e.target.files]);
  e.target.value = "";
};
$("#files").ondragover = (e) => {
  e.preventDefault();
  $("#files").classList.add("drop");
};
$("#files").ondragleave = () => $("#files").classList.remove("drop");
$("#files").ondrop = (e) => {
  e.preventDefault();
  $("#files").classList.remove("drop");
  upload([...e.dataTransfer.files]);
};

// --- terminal ---
// The session cookie rides along on the upgrade; the server checks Origin.
let tty = null;
$("#a-term").onclick = () => {
  const id = sel;
  const term = new Terminal({ cursorBlink: true, fontFamily: "ui-monospace, Menlo, monospace", fontSize: 13, theme: { background: "#0a0a0a" } });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  $("#t-id").textContent = id;
  $("#t-state").textContent = "connecting…";
  $("#term").hidden = false;
  term.open($("#xterm"));
  fit.fit();
  const ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/sandboxes/${id}/terminal`, ["dawnbx"]);
  ws.binaryType = "arraybuffer";
  const enc = new TextEncoder();
  const send = (d) => ws.readyState === WebSocket.OPEN && ws.send(d);
  const size = () => send(JSON.stringify({ cols: term.cols, rows: term.rows }));
  ws.onopen = () => {
    $("#t-state").textContent = "";
    size();
    term.focus();
  };
  ws.onmessage = (e) => term.write(new Uint8Array(e.data));
  ws.onclose = (e) => {
    if (tty?.ws !== ws) return;
    $("#t-state").textContent = e.code === 1000 ? "shell exited" : `disconnected: ${e.reason || "is the sandbox running and are you signed in?"}`;
  };
  term.onData((d) => send(enc.encode(d)));
  term.onResize(size);
  const ro = new ResizeObserver(() => fit.fit());
  ro.observe($("#xterm"));
  tty = { ws, term, ro };
};
$("#t-close").onclick = () => {
  const t = tty;
  tty = null;
  $("#term").hidden = true;
  t?.ro.disconnect();
  t?.ws.close();
  t?.term.dispose();
};

// --- boot ---
let timer;
function start() {
  $("#me").textContent = me.user;
  $("#login").hidden = true;
  $("#app").hidden = false;
  refresh();
  clearInterval(timer);
  timer = setInterval(refresh, 2000);
}
api("GET", "/v1/me")
  .then((p) => {
    if (!p.user) throw new Error("not a dashboard session");
    me = p;
    start();
  })
  .catch(() => signedOut());
