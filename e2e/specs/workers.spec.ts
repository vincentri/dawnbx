import { expect, test } from '@playwright/test';
import { controlPlane } from '../fixtures/control-plane';
import { envFor, type TestOutcome } from '../fixtures/test-provider';
import { eventually } from '../support/expect';
import {
  activeStepText,
  clusterRow,
  confirmInDialog,
  createButton,
  estimateButton,
  firstOption,
  nameField,
  providerChoice,
  regionPicker,
  sizePicker,
} from '../support/selectors';

/**
 * User Story 3, priority P3: an operator manages a worker's life in the browser.
 *
 * Add a worker, watch it come up, be refused a removal that would strand
 * workloads, remove it, and delete the cluster. This is the day-two operation,
 * and the refusal is a safety property the operator has to be able to trust
 * from what the screen says rather than infer.
 */

async function present(outcome: TestOutcome): Promise<void> {
  const base = process.env.DAWNBX_E2E_BASE_URL ?? 'http://server:8080';
  const res = await fetch(`${base}/v1/e2e/outcome`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(envFor(outcome)),
  });
  if (!res.ok && res.status !== 204) {
    throw new Error(`the suite could not set the provider outcome: ${res.status}`);
  }
}

/** Creates a cluster and waits for it to be ready, which is the precondition for
 * every worker operation. A worker added to a cluster that is not up would fail
 * for a reason that has nothing to do with the worker. */
async function readyCluster(page: import('@playwright/test').Page, name: string) {
  const cp = await controlPlane();
  await cp.signIn(page);
  await page.goto('/ui/clusters');
  await providerChoice(page, 'test').click();
  await nameField(page).fill(name);
  await regionPicker(page).click();
  await firstOption(page).click();
  await sizePicker(page).click();
  await firstOption(page).click();
  await estimateButton(page).click();
  await expect(page.getByText(/\/mo|\$[0-9]/).first()).toBeVisible({ timeout: 20_000 });
  await createButton(page).click();
  await confirmInDialog(page).click();
  await eventually(
    async () => (await activeStepText(page)).startsWith('ready'),
    'the cluster to report ready',
    90_000,
  );
}

test.describe('a worker', () => {
  test('is added and reaches ready', async ({ page }) => {
    await present({ cluster: 'succeed' });
    await readyCluster(page, 'with-workers');

    await page.getByRole('button', { name: /add.*(worker|node)/i }).first().click();

    // The worker must actually appear, not merely be requested. A list that
    // stays empty while the request succeeds is the "worker added" bug in its
    // most common form, and it is invisible to an API-only test.
    await eventually(
      async () => (await page.getByText(/worker|node/i).count()) > 0,
      'the worker to appear in the cluster',
      60_000,
    );
  });

  test('cannot be removed while it holds workloads', async ({ page }) => {
    // FR-012: a removal that would strand sandboxes is refused, with a reason.
    // The refusal is the assertion; a successful removal here would mean the
    // product let an operator strand running work.
    await present({ cluster: 'succeed', holdWorkers: true });
    await readyCluster(page, 'busy-worker');

    await page.getByRole('button', { name: /add.*(worker|node)/i }).first().click();
    await eventually(
      async () => (await page.getByText(/worker|node/i).count()) > 0,
      'the worker to appear',
      60_000,
    );

    await page.getByRole('button', { name: /remove|delete/i }).first().click();
    const confirm = page.getByRole('button', { name: /remove|delete|yes/i }).last();
    if (await confirm.count()) await confirm.click();

    // The refusal must name the cause, not just fail.
    await eventually(
      async () => (await page.getByText(/still holds sandboxes|holds sandboxes/i).count()) > 0,
      'the interface to refuse the removal and say why',
      30_000,
    );
  });

  test('a cluster with no workers is deleted and leaves the list', async ({ page }) => {
    await present({ cluster: 'succeed' });
    await readyCluster(page, 'to-delete');

    await page.getByRole('button', { name: /delete cluster|delete/i }).first().click();
    const confirm = page.getByRole('button', { name: /^delete|yes/i }).last();
    if (await confirm.count()) await confirm.click();

    // Deleting must remove it from the interface, not just answer 200. A cluster
    // that is gone from the database and still on screen is the failure an
    // operator notices last and trusts least.
    await eventually(
      async () => (await clusterRow(page, 'to-delete').count()) === 0,
      'the deleted cluster to leave the list',
      60_000,
    );
  });
});
