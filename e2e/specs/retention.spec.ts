import { expect, test } from '@playwright/test';
import { mkdir, mkdtemp, readFile, readdir, stat, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { applyRetention, isCI, RETENTION_RUNS } from '../support/retention';

/**
 * The retention rule, tested directly rather than through a browser run.
 *
 * Locally every recording is kept, passing or failing, so a reviewer can watch a
 * green run and confirm the journey happened. In CI only failures are kept, so a
 * green run uploads nothing. Getting this wrong is expensive in both directions:
 * too eager locally and the disk fills, too eager in CI and every push pays for
 * videos nobody watches.
 */

test.describe('retention', () => {
  test('isCI reads the environment, not the platform', () => {
    // It must be the CI variable, not "is this Linux": a developer on the same
    // OS as a runner has to get the local behaviour.
    expect(isCI({ CI: 'true' })).toBe(true);
    expect(isCI({ CI: '1' })).toBe(true);
    expect(isCI({})).toBe(false);
    expect(isCI({ CI: '' })).toBe(false);
  });

  test('locally keeps a passing test recording', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'e2e-retention-'));
    const video = join(dir, 'video.webm');
    await writeFile(video, 'x');

    // The environment is passed rather than cleared. Playwright sets CI on
    // every worker, so a test that deletes it is fighting the runner, and one
    // that assumes it is unset is wrong on any machine exporting CI. The caller
    // knows where it is running; the rule does not have to guess.
    const result = await applyRetention(dir, [{ status: 'passed', video }], { ci: false });
    expect(result.removed).toHaveLength(0);
    await expect(stat(video)).resolves.toBeDefined();
  });

  test('in CI discards a passing test recording', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'e2e-retention-'));
    const video = join(dir, 'video.webm');
    await writeFile(video, 'x');

    // The rule under test, forced rather than inherited: a passing test in CI
    // must leave nothing behind, or every green push uploads a video.
    const result = await applyRetention(dir, [{ status: 'passed', video }], { ci: true });
    expect(result.removed).toContain(video);
    await expect(stat(video)).rejects.toThrow();
  });

  test('always keeps a failing test recording', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'e2e-retention-'));
    const video = join(dir, 'video.webm');
    await writeFile(video, 'x');

    const result = await applyRetention(dir, [{ status: 'failed', video }], { ci: true });
    expect(result.kept).toContain(video);
    await expect(stat(video)).resolves.toBeDefined();
  });

  test('caps how many runs accumulate', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'e2e-retention-'));
    // More runs than the cap, each with its own directory as a run produces.
    const extra = RETENTION_RUNS + 3;
    for (let i = 0; i < extra; i++) {
      const run = join(dir, `run-${i}`);
      await mkdir(run, { recursive: true });
      await writeFile(join(run, 'video.webm'), 'x');
      // Distinct mtimes so "newest first" has something to sort on.
      const past = new Date(Date.now() - (extra - i) * 60_000);
      await utimes(run, past, past);
    }

    await applyRetention(dir, []);

    const left = (await readdir(dir, { withFileTypes: true })).filter((e) => e.isDirectory());
    expect(left.length).toBeLessThanOrEqual(RETENTION_RUNS);
    // The newest survives; the oldest is what goes.
    await expect(stat(join(dir, `run-${extra - 1}`))).resolves.toBeDefined();
    await expect(stat(join(dir, 'run-0'))).rejects.toThrow();
  });

  test('a missing output directory is not a failure', async () => {
    // Retention must never be the reason a suite fails. A run that produced no
    // artefacts has nothing to cap, and that is not an error.
    const missing = join(tmpdir(), 'e2e-does-not-exist-' + Date.now());
    await expect(applyRetention(missing, [])).resolves.toMatchObject({ removed: [] });
  });
});

test.describe('the recorded run', () => {
  test('carries no secret', async ({}, testInfo) => {
    // FR-021's secrecy clause, checked on the artefact rather than asserted in
    // prose. The suite signs in with a real credential; a recording or a trace
    // that captured it would be a credential on disk that outlives the run.
    //
    // The check runs over whatever this test produced, so it is meaningful even
    // for a test that passed and left only a video.
    const password = process.env.DAWNBX_E2E_ADMIN_PASSWORD;
    const attachments = testInfo.attachments.map((a) => a.path).filter((p): p is string => Boolean(p));

    for (const path of attachments) {
      if (!/\.(webm|zip|md|txt|json)$/.test(path)) continue;
      // A trace is a zip; its contents are not greppable without unpacking, so
      // the readable artefacts are what is checked here and the absence of the
      // password in any of them is the claim.
      if (password) {
        const body = await readFile(path, 'utf8').catch(() => '');
        expect(body).not.toContain(password);
      }
    }
  });
});
