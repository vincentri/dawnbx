import path from "node:path";
import { defineConfig } from "vitest/config";

// The dashboard's only testable-without-a-DOM surface is the API client, so the
// suite runs in the node environment. Component tests would need jsdom, which is
// not installed; coverage below is API-client coverage, not UI coverage.
export default defineConfig({
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
    coverage: {
      provider: "v8",
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/lib/schema.d.ts", "src/main.tsx", "**/*.test.ts"],
      thresholds: { lines: 95, functions: 95, statements: 95, branches: 95 },
    },
  },
});
