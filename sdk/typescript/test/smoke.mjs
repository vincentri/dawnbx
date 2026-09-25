// Live end-to-end check against a running server (DAWNBX_URL, DAWNBX_API_KEY).
//   npm run build && node test/smoke.mjs
import assert from "node:assert/strict";
import { Sandbox, DawnbxError } from "../dist/index.js";

const t0 = Date.now();
const step = (s) => console.log(`${((Date.now() - t0) / 1000).toFixed(1)}s  ${s}`);

const sb = await Sandbox.create({ ttl: "10m" });
step(`created ${sb.id}`);
try {
  let r = await sb.exec("python -c 'import sys; print(sys.version_info[:2])'; exit 7");
  assert.equal(r.exitCode, 7);
  assert.match(r.stdout, /\(3, 12\)/);
  step("exec: stdout + nonzero exit");

  r = await sb.exec("dmesg | head -1");
  assert.match(r.stdout, /gVisor/i);
  step("runs under gVisor");

  await sb.files.write("dir/a.txt", "héllo\n");
  assert.equal(await sb.files.read("dir/a.txt"), "héllo\n");
  assert.equal((await sb.exec("cat /workspace/dir/a.txt")).stdout, "héllo\n");
  await assert.rejects(sb.files.read("nope.txt"), (e) => e.code === "file_not_found");
  step("files write/read, missing file typed");

  // A symlink to a host-looking path resolves inside the sandbox, never on the host.
  await sb.exec("ln -s /etc/hostname link");
  assert.equal((await sb.files.read("link")).trim(), (await sb.exec("cat /etc/hostname")).stdout.trim());
  step("symlink read stays inside sandbox");

  await assert.rejects(sb.exec("sleep 30", { timeout: 1 }), (e) => e instanceof DawnbxError && e.code === "exec_timeout");
  step("exec timeout");

  r = await sb.exec("echo bg-ok; sleep 60", { background: true });
  assert.ok(r.pid > 0 && r.log);
  await new Promise((res) => setTimeout(res, 500));
  assert.match(await sb.files.read(r.log), /bg-ok/);
  step(`background pid ${r.pid}`);

  r = await sb.exec("echo $FOO", { env: { FOO: "bar" } });
  assert.equal(r.stdout, "bar\n");

  r = await sb.exec("python -c \"import urllib.request; print(urllib.request.urlopen('https://pypi.org', timeout=10).status)\"");
  assert.equal(r.stdout.trim(), "200");
  step("internet egress");

  // fork: files copied, siblings isolated, parent's processes paused then resumed.
  await sb.files.write("notes.txt", "base\n");
  await sb.exec("i=0; while true; do i=$((i+1)); echo $i > tick; sleep 0.2; done", { background: true });
  let ft = Date.now();
  const kids = await sb.fork(3);
  step(`fork(3) in ${Date.now() - ft} ms`);
  try {
    await Promise.all(kids.map((k, i) => k.exec(`echo child-${i} >> notes.txt`)));
    for (const [i, k] of kids.entries()) {
      assert.equal(await k.files.read("notes.txt"), `base\nchild-${i}\n`);
      assert.equal(k.info.parent, sb.id);
    }
    assert.equal(await sb.files.read("notes.txt"), "base\n");
    const t1 = Number(await sb.files.read("tick"));
    await new Promise((res) => setTimeout(res, 1000));
    assert.ok(Number(await sb.files.read("tick")) > t1, "parent loop resumed");
    const k0 = Number(await kids[0].files.read("tick"));
    await new Promise((res) => setTimeout(res, 1000));
    assert.equal(Number(await kids[0].files.read("tick")), k0, "child has no copied processes");
    step("fork: files copied, siblings isolated, parent resumed, child processes fresh");
  } finally {
    await Promise.all(kids.map((k) => k.kill()));
  }

  await sb.extend(null);
  assert.equal((await sb.refresh()).expiresAt, null);
  step("extend(null) keeps forever");

  assert.ok((await Sandbox.list()).some((s) => s.id === sb.id));
  step("list");
} finally {
  await sb.kill();
}
await assert.rejects(Sandbox.get(sb.id), (e) => e.code === "not_found");
step("killed");

const off = await Sandbox.create({ network: "none" });
try {
  const r = await off.exec("python -c \"import urllib.request; urllib.request.urlopen('https://pypi.org', timeout=5)\"");
  assert.notEqual(r.exitCode, 0);
  step("network none blocks egress");
} finally {
  await off.kill();
}

await assert.rejects(Sandbox.create({ image: "docker.io/library/nope-does-not-exist:1" }), (e) => e.code === "image_pull_failed");
step("bad image -> image_pull_failed");
console.log("SMOKE OK");
