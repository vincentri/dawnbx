# Stack and technology preferences

- Go for the server and CLI. Small binaries, standard library where possible, `CGO_ENABLED=0`
  for cross-compiling. Confidence: 0.9
- k3s (server) and gVisor/runsc (sandbox runtime) is the chosen stack. gVisor over Firecracker
  because it needs no KVM, so it runs on cheap VPS and on a Mac dev box. Confidence: 0.9
- TypeScript SDK before Python: "for first sdk, can we support typescript forst".
  Python is the deferred second SDK. Confidence: 0.85
- Wants "proper", modern, mainstream libraries rather than hand-rolled: asked for React + Vite,
  then corrected to "use proper framework for drontend" (TanStack Router + Query, Tailwind v4,
  shadcn/ui, openapi-typescript + openapi-fetch for a typed client). "use proper oen la" is a
  recurring ask. Confidence: 0.9
- Serves a real framework build: the Go binary embeds the compiled SPA rather than keeping
  hand-written vanilla JS, so no Node runtime is needed on servers. Confidence: 0.8
- Prioritises speed and usability over theoretical isolation: "i just need the fast one and usable
  one. the besst in market". Chose a warm pod pool over microVMs for latency. Confidence: 0.85
- Prefers boring, portable choices over cloud-locked ones: SQLite by default with Postgres
  available for scale, rather than requiring a managed DB. Rejected replicated storage and
  vendor-specific services (EFS) with reasons. Confidence: 0.85
- One binary, many surfaces: CLI + SDKs + dashboard + control plane from a single Go tree. Confidence: 0.7
- Uses Yapp as a build/hosting target (has a site on a `*.yapp.ink` subdomain), so Yapp may be a
  deployment or marketing surface worth remembering. Confidence: 0.5
- Minimal-code bias is explicit ("ponytail" mode): prefers the simplest thing that works, with
  a comment explaining the ceiling rather than building the abstraction now. Confidence: 0.9
