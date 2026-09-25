import path from "node:path";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The Go server embeds the build (internal/api/ui) and serves it at /ui/.
// `npm run dev` proxies /v1 to a local dawnbx-server (hack/dev-vm.sh forwards 8080).
export default defineConfig({
  base: "/ui/",
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  build: { outDir: "../internal/api/ui", emptyOutDir: true },
  server: { proxy: { "/v1": { target: "http://127.0.0.1:8080", ws: true } } },
});
