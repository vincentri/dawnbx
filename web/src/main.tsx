import "./index.css";
import { MutationCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createRootRoute, createRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { toast } from "sonner";
import { IndexPage } from "@/components/index-page";
import { Toaster } from "@/components/ui/sonner";
import { setSignedOut } from "@/lib/api";
import { Clusters, clusterSearch } from "@/pages/clusters";
import { Settings } from "@/pages/settings";
import { Shell, signOut } from "@/pages/shell";
import { TerminalPage } from "@/pages/terminal";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, refetchOnWindowFocus: true } },
  // Every failed action shows as a toast; the id dedups repeats.
  mutationCache: new MutationCache({ onError: (e) => toast.error(e.message, { id: e.message }) }),
});
setSignedOut(() => signOut(queryClient));

const root = createRootRoute({ component: Shell });
const routeTree = root.addChildren([
  // The index is the sandboxes list, which is right on a cluster and an error
  // page on a control plane: /v1/sandboxes is a hard 503 there. IndexPage picks
  // the right one for the server it is talking to, rather than the nav having to
  // remember.
  createRoute({
    getParentRoute: () => root,
    path: "/",
    component: IndexPage,
    validateSearch: (s: Record<string, unknown>): { id?: string } =>
      typeof s.id === "string" ? { id: s.id } : {},
  }),
  createRoute({
    getParentRoute: () => root,
    path: "/clusters",
    component: Clusters,
    validateSearch: clusterSearch,
  }),
  createRoute({
    getParentRoute: () => root,
    path: "/settings",
    component: Settings,
    validateSearch: (s: Record<string, unknown>): { tab?: string } =>
      typeof s.tab === "string" ? { tab: s.tab } : {},
  }),
  createRoute({ getParentRoute: () => root, path: "/terminal/$id", component: TerminalPage }),
]);
const router = createRouter({ routeTree, basepath: "/ui" });
declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster position="bottom-right" />
    </QueryClientProvider>
  </StrictMode>,
);
