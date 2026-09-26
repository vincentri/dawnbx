// Create/exec/fork latency against a running server (DAWNBX_URL, DAWNBX_API_KEY).
//   npm run build && node test/bench.mjs
import { Sandbox } from "../dist/index.js"

const since = (t) => `${Date.now() - t} ms`
for (let i = 0; i < 3; i++) {
  let t = Date.now()
  const sb = await Sandbox.create()
  const created = since(t)
  t = Date.now()
  await sb.exec("echo hi")
  const exec = since(t)
  t = Date.now()
  const kids = await sb.fork(1)
  console.log(`create ${created}  first exec ${exec}  fork(1) ${since(t)}`)
  await Promise.all([...kids, sb].map((s) => s.kill()))
  await new Promise((r) => setTimeout(r, 15000)) // let the pool refill
}
