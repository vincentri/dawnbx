import { readFile } from 'node:fs/promises';
import { join } from 'node:path';

/**
 * The control plane under test, and the credential needed to sign in to it.
 *
 * The credential is read from the file the control plane itself wrote, which is
 * how an operator's first sign-in actually works (research.md R-007). It is
 * never logged, never asserted on, and never written to a report: a recording
 * that captured it would be a credential on disk that outlives the run.
 */

/** Where the control plane keeps its per-run files inside the container. */
export const DATA_DIR = process.env.DAWNBX_E2E_DATA_DIR ?? '/data';

/** The account the control plane creates on first start. */
export const ADMIN_USER = process.env.DAWNBX_ADMIN_USER ?? 'admin';

export interface ControlPlane {
  baseURL: string;
  /** Signs in through the real form and returns the password used. */
  signIn: (page: import('@playwright/test').Page) => Promise<void>;
}

async function readAdminPassword(): Promise<string> {
  const path = join(DATA_DIR, 'server', 'admin.env');
  const raw = await readFile(path, 'utf8');
  for (const line of raw.split('\n')) {
    const eq = line.indexOf('=');
    if (eq > 0 && line.slice(0, eq).trim() === 'DAWNBX_ADMIN_PASSWORD') {
      return line.slice(eq + 1).trim();
    }
  }
  throw new Error(`no administrator password in ${path}; the control plane did not mint one`);
}

/** controlPlane describes the running server to the suite. */
export async function controlPlane(): Promise<ControlPlane> {
  const baseURL = process.env.DAWNBX_E2E_BASE_URL ?? 'http://server:8080';
  return {
    baseURL,
    signIn: async (page) => {
      // Read at use time, never at module load: the server may not have written
      // the file yet when this module is first evaluated.
      const password = await readAdminPassword();
      await page.goto('/ui/');
      // The form asks for a username as well; the control plane's own default is
      // what an operator types, and getting this wrong would make the suite fail
      // for a reason that has nothing to do with the code under test.
      await page.getByLabel(/username/i).fill(ADMIN_USER);
      await page.getByLabel(/password/i).fill(password);
      await page.getByRole('button', { name: /sign in/i }).click();
      // Landing on the shell is the assertion. The password never appears in a
      // locator, a message, or a trace of this call.
      await page.waitForURL(/\/ui\/(\/clusters)?(\?.*)?$/, { timeout: 30_000 });
    },
  };
}
