import type { Dirent } from 'node:fs';
import { readdir, rm, stat } from 'node:fs/promises';
import { join } from 'node:path';

/**
 * Applies the retention rule after a run.
 *
 * Locally every recording is kept, passing or failing, so a reviewer can watch a
 * green run and confirm the journey actually happened rather than trusting the
 * result. In CI only failures are kept, so a green run uploads nothing at all.
 *
 * This runs once at the end rather than configuring Playwright differently per
 * environment, because two configurations drift and "a green CI run uploads
 * nothing" has to be enforced rather than remembered (FR-023, NFR-007).
 */

/** How many failed runs to keep before the oldest is removed. */
export const RETENTION_RUNS = 5;

export interface RetentionResult {
  kept: string[];
  removed: string[];
}

/** isCI reports whether this run should keep failures only. */
export function isCI(env: NodeJS.ProcessEnv = process.env): boolean {
  return Boolean(env.CI);
}

/**
 * applyRetention keeps or discards per-test recordings according to the
 * environment, then caps how many runs accumulate.
 *
 * @param outputDir the directory Playwright wrote into
 * @param results one entry per finished test
 */
export async function applyRetention(
  outputDir: string,
  results: readonly { status: string; video?: string }[],
  // The environment is a parameter, not a lookup, so the rule is decidable by a
  // caller that knows where it is running. Reading process.env here would make
  // the behaviour depend on whatever the runner happened to export.
  env: { ci?: boolean } = {},
): Promise<RetentionResult> {
  const ci = env.ci ?? isCI();
  const kept: string[] = [];
  const removed: string[] = [];

  for (const r of results) {
    if (!r.video) continue;
    const passed = r.status === 'passed' || r.status === 'skipped';
    if (ci && passed) {
      await rm(r.video, { force: true });
      removed.push(r.video);
    } else {
      kept.push(r.video);
    }
  }

  // The cap applies in both environments: a machine that keeps every local run
  // forever is the same unbounded growth in slower clothing (FR-024, NFR-005).
  const capped = await capOldestRuns(outputDir, removed);
  removed.push(...capped);
  return { kept, removed };
}

/**
 * capOldestRuns keeps at most RETENTION_RUNS test directories, newest first.
 * A run is one top-level directory Playwright created for a test.
 */
async function capOldestRuns(
  outputDir: string,
  alreadyRemoved: string[],
): Promise<string[]> {
  let entries: Dirent[];
  try {
    entries = await readdir(outputDir, { withFileTypes: true });
  } catch {
    return []; // no output directory yet; nothing to cap
  }
  const dirs = entries.filter((entry) => entry.isDirectory()).map((entry) => String(entry.name));
  if (dirs.length <= RETENTION_RUNS) return [];

  // Newest first, by mtime. Any error reading a timestamp sorts it last rather
  // than failing the run: retention must never be the reason a suite fails.
  const withTime = await Promise.all(
    dirs.map(async (name) => {
      try {
        const s = await stat(join(outputDir, name));
        return { name, mtime: s.mtimeMs };
      } catch {
        return { name, mtime: 0 };
      }
    }),
  );
  withTime.sort((a, b) => b.mtime - a.mtime);

  const doomed = withTime.slice(RETENTION_RUNS).map((entry) => entry.name);
  for (const name of doomed) {
    await rm(join(outputDir, name), { recursive: true, force: true });
  }
  return doomed.map((n) => join(outputDir, n));
}
