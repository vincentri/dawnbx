import Link from 'next/link';

export default function HomePage() {
  return (
    <main className="mx-auto flex max-w-3xl flex-col px-4 py-16">
      <h1 className="text-4xl font-bold">dawnbx</h1>
      <p className="mt-4 text-lg text-fd-muted-foreground">
        Self-hosted sandboxes for AI agents on one Linux box. Each sandbox is a gVisor container on
        k3s with its own <code>/workspace</code> — run commands in it, read and write files in it,
        and fork it. You get an HTTP API, a CLI, Python and TypeScript SDKs, and a web dashboard.
      </p>

      <div className="mt-8 flex flex-wrap gap-3">
        <Link
          href="/docs"
          className="rounded-md bg-fd-primary px-4 py-2 text-sm font-medium text-fd-primary-foreground"
        >
          Read the docs
        </Link>
        <Link href="/docs/start/installation" className="rounded-md border px-4 py-2 text-sm font-medium">
          Install
        </Link>
        <Link href="/docs/guide/api" className="rounded-md border px-4 py-2 text-sm font-medium">
          API
        </Link>
      </div>

      <pre className="mt-10 overflow-x-auto rounded-lg border p-4 text-sm">
        <code>{`id=$(dawnbx create) && dawnbx exec $id "python -c 'print(6*7)'" && dawnbx kill $id`}</code>
      </pre>
    </main>
  );
}
