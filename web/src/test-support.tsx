// Test-only helper: the app's real router + query client, so page tests
// exercise the same search-param plumbing the browser does. The network
// boundary lives in ./test-fetch.
import { MutationCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { type RenderResult, render } from "@testing-library/react";
import { toast } from "sonner";
import { Toaster } from "@/components/ui/sonner";
import { type Principal, setSignedOut } from "@/lib/api";
import { Sandboxes } from "@/pages/sandboxes";
import { Settings } from "@/pages/settings";
import { Shell, signOut } from "@/pages/shell";
import { TerminalPage } from "@/pages/terminal";

export const ME: Principal = { user: "ada", org: "acme", admin: false };
export const ADMIN: Principal = { user: "root", org: "acme", admin: true };
export const STATUS = {
  version: "1.4.0",
  free_pct: 42.4,
  warm: 1,
  pool_size: 4,
};

/** The routes Shell and each page expect, over an in-memory history. */
export function renderApp(
  opts: { entry?: string; me?: Principal | null } = {},
): RenderResult & { qc: QueryClient } {
  const qc = new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        gcTime: Number.POSITIVE_INFINITY,
        refetchOnWindowFocus: false,
      },
      mutations: { retry: false },
    },
    // main.tsx funnels every failed action into a toast; mirror that so these
    // tests see the same thing a user would.
    mutationCache: new MutationCache({
      onError: (e) => toast.error(e.message, { id: e.message }),
    }),
  });
  // Priming ["me"] is what a resolved /v1/me leaves behind; staleTime is
  // Infinity, so the query will not refetch over it.
  if (opts.me !== undefined) qc.setQueryData(["me"], opts.me);
  setSignedOut(() => signOut(qc));

  const rootRoute = createRootRoute({ component: Shell });
  const routeTree = rootRoute.addChildren([
    createRoute({
      getParentRoute: () => rootRoute,
      path: "/",
      component: Sandboxes,
      validateSearch: (s: Record<string, unknown>): { id?: string } =>
        typeof s.id === "string" ? { id: s.id } : {},
    }),
    createRoute({
      getParentRoute: () => rootRoute,
      path: "/settings",
      component: Settings,
      validateSearch: (s: Record<string, unknown>): { tab?: string } =>
        typeof s.tab === "string" ? { tab: s.tab } : {},
    }),
    createRoute({
      getParentRoute: () => rootRoute,
      path: "/terminal/$id",
      component: TerminalPage,
    }),
  ]);
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [opts.entry ?? "/"] }),
  });

  const out = render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
      <Toaster position="bottom-right" />
    </QueryClientProvider>,
  );
  return Object.assign(out, { qc });
}
