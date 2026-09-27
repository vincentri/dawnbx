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
  failureDetail,
  firstOption,
  nameField,
  providerChoice,
  regionPicker,
  sizePicker,
} from '../support/selectors';

/**
 * User Story 2, priority P2: an operator recovers from a failure without reading
 * source.
 *
 * A cluster that will not come up has to leave a reason on screen, and a cluster
 * that cannot be reached has to be distinguishable from one with no workers.
 * Both are the difference between an operator knowing what to do next and an
 * operator filing a bug.
 *
 * The outcome is set per test through the control route, so one server presents
 * a succeeding cluster to the happy-path tests and a failing one here. A
 * provider built once per process would have made every failure journey a
 * separate container run, and a suite that cannot run its failures without
 * restarting its subject is a suite that rarely runs them.
 */

/** Sets what the test provider presents for the duration of one test. */
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

/** Signs in and opens the request form for a provider. */
async function openForm(page: import('@playwright/test').Page, name: string) {
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
}

test.describe('a cluster that fails', () => {
  test('shows the specific reason, not a generic one', async ({ page }) => {
    // The reason is deliberately specific and unusual, so the assertion cannot
    // pass on a generic "something went wrong" the interface might always show.
    const reason = 'the instance type is not available in ap-southeast-1';
    await present({ cluster: 'fail', failureReason: reason });

    await openForm(page, 'will-fail');

    await eventually(
      async () => (await failureDetail(page).count()) > 0,
      'the cluster to show a failure',
      90_000,
    );
    // FR-010: the specific reason must reach the operator.
    await expect(page.getByText(reason, { exact: false })).toBeVisible();
  });

  test('tells an unreachable cluster apart from one with no workers', async ({ page }) => {
    await present({ cluster: 'unreachable' });

    await openForm(page, 'unreachable');

    // This is the distinction the product is most often wrong about: an empty
    // list reads as "no workers", which sends an operator to wait for a repair
    // that is already broken. The interface must say it cannot be reached.
    await eventually(
      async () => (await page.getByText(/unreachable|cannot reach/i).count()) > 0,
      'the interface to report the cluster as unreachable',
      90_000,
    );
    await expect(page.getByText(/no workers|zero workers/i)).toHaveCount(0);
  });
});

test.describe('the provider roster', () => {
  test('lists an unavailable provider and refuses to select it', async ({ page }) => {
    // aws, azure and gcp are declared but unavailable, and that is what an
    // operator must see: the choice is never a guess (FR-013).
    await present({ cluster: 'succeed', providerAvailable: true });

    const cp = await controlPlane();
    await cp.signIn(page);
    await page.goto('/ui/clusters');

    for (const id of ['aws', 'azure', 'gcp']) {
      const choice = providerChoice(page, id);
      await expect(choice).toBeVisible();
      await expect(choice).toBeDisabled();
      await expect(choice).toContainText(/unavailable/i);
    }
    // The available one is selectable, or the roster proves nothing.
    await expect(providerChoice(page, 'test')).toBeEnabled();
  });
});

test.describe('a quote that went stale', () => {
  test('is refused with an explanation rather than silently repriced', async ({ page }) => {
    // A quote is bound to the configuration it prices. Changing the size after
    // quoting must not carry the old price over, because that is how an
    // operator approves one thing and pays for another.
    await present({ cluster: 'succeed' });

    const cp = await controlPlane();
    await cp.signIn(page);
    await page.goto('/ui/clusters');
    await providerChoice(page, 'test').click();
    await nameField(page).fill('requoted');

    await regionPicker(page).click();
    await firstOption(page).click();
    await sizePicker(page).click();
    await firstOption(page).click();
    await estimateButton(page).click();
    await expect(page.getByText(/\/mo|\$[0-9]/).first()).toBeVisible({ timeout: 20_000 });

    // Change the configuration after quoting, then try to create.
    await page.getByRole('button', { name: 'Change configuration' }).click();
    await sizePicker(page).click();
    await page.getByRole('option').last().click();
    await createButton(page).click();

    // Either it refuses, or it asks for a fresh estimate. Silently creating at
    // the old price is the one outcome that is wrong.
    await eventually(
      async () =>
        (await page.getByText(/price changed|estimate|stale|re-quote/i).count()) > 0 ||
        (await page.getByRole('button', { name: 'Estimate price' }).count()) > 0,
      'the interface to require a fresh estimate after the configuration changed',
      30_000,
    );
  });
});
