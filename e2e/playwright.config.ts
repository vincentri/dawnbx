import { defineConfig } from '@playwright/test';

// The suite runs entirely in Docker Compose (specs/002-control-plane-ui-e2e/plan.md).
// Nothing here reaches a cloud, and nothing is installed on the host: the browser
// lives in the image. `.e2e/` is bind-mounted out of the container because the
// recordings are this feature's output (R-009).
//
// Two settings are load-bearing and must not be "improved" later:
//
//   retries: 0   SC-002 claims 20 consecutive runs agree. A retry would hide
//                exactly the flake that claim exists to detect.
//   video: 'on'  every run records, and retention is applied afterwards by
//                support/retention.ts: all of it locally, failures only in CI
//                (FR-023). Configuring video per environment instead would make
//                "a green CI run uploads nothing" something to remember rather
//                than something enforced.
// Where recordings land. It differs between the two environments because only the
// container can see the bind mount: on a developer machine the suite would
// normally run in the image, but the config also has to work for a bare run.
const outputDir = process.env.DAWNBX_E2E_OUTPUT_DIR ?? '../.e2e';

export default defineConfig({
  testDir: './specs',
  outputDir,
  // No retries: see above. A skipped test fails the run (FR-022), so a suite that
  // quietly skips a broken test would report green while covering nothing.
  retries: 0,
  fullyParallel: false,
  // One worker: the control plane under test is a single process with a single
  // database, and NFR-001 budgets 3 minutes. Parallel workers would contend for
  // it and turn a timing question into a scheduling one.
  workers: 1,
  forbidOnly: !!process.env.CI,
  // The report lives beside the recordings, not inside them: the HTML reporter
  // clears its own output folder, and nesting it under test-results would have
  // it delete the videos it is meant to describe.
  reporter: process.env.CI
    ? [['list'], ['html', { outputFolder: `${outputDir}-report`, open: 'never' }]]
    : [['list']],
  use: {
    // The server service, by name on the Compose network.
    baseURL: process.env.DAWNBX_E2E_BASE_URL ?? 'http://server:8080',
    video: 'on',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
});
