import "./index.css";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MutationCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createRootRoute, createRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { toast } from "sonner";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { setSignedOut } from "@/lib/api";
import { Shell } from "@/pages/shell";
import { Sandboxes } from "@/pages/sandboxes";
import { Settings } from "@/pages/settings";
import { TerminalPage } from "@/pages/terminal";

export const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, refetchOnWindowFocus: true } },
  // Every failed action shows as a toast; the id dedups repeats.
  mutationCache: new MutationCache({ onError: (e) => toast.error(e.message, { id: e.message }) }),
});
setSignedOut(() => queryClient.setQueryData(["me"], null));

const root = createRootRoute({ component: Shell });
const routeTree = root.addChildren([
  createRoute({
    getParentRoute: () => root,
    path: "/",
    component: Sandboxes,
    validateSearch: (s: Record<string, unknown>): { id?: string } => (typeof s.id === "string" ? { id: s.id } : {}),
  }),
  createRoute({
    getParentRoute: () => root,
    path: "/settings",
    component: Settings,
    validateSearch: (s: Record<string, unknown>): { tab?: string } => (typeof s.tab === "string" ? { tab: s.tab } : {}),
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
      <TooltipProvider>
        <RouterProvider router={router} />
        <Toaster position="bottom-right" />
      </TooltipProvider>
    </QueryClientProvider>
  </StrictMode>,
);
