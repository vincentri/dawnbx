import path from "node:path";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// Node env is the default so `src/lib` tests stay fast; component tests opt
// into jsdom with a `// @vitest-environment jsdom` docblock.
export default defineConfig({
  plugins: [react()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "src") } },
  test: {
    // RTL registers its own afterEach(cleanup) when globals are on.
    globals: true,
    environment: "node",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["src/test-setup.ts"],
    restoreMocks: true,
    coverage: {
      provider: "v8",
      include: ["src/lib/**/*.ts", "src/pages/**/*.tsx", "src/components/**/*.tsx"],
      exclude: ["src/lib/schema.d.ts"],
      reporter: ["text", "json-summary"],
    },
  },
});
