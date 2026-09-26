import path from "node:path";
import { defineConfig } from "vitest/config";

// Coverage is measured over src/lib only, which is the dashboard's own logic.
// The shadcn components under src/components/ui and the page components are
// vendored or thin wrappers: testing them would test Radix, and 95% of another
// library's source is not a quality signal about this repo. The thresholds are
// read from hack/coverage-floor.txt by hack/check.sh, which passes them in, so
// this file does not carry a second copy of the numbers.
export default defineConfig({
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
    coverage: {
      provider: "v8",
      include: ["src/lib/**/*.ts"],
      exclude: ["src/lib/schema.d.ts", "**/*.test.ts"],
    },
  },
});
