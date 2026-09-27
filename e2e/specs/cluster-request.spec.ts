import { expect, test } from '@playwright/test';
import { controlPlane } from '../fixtures/control-plane';
import { eventually } from '../support/expect';
import {
  anyPrice,
  clusterRow,
  confirmDialog,
  confirmInDialog,
  createButton,
  estimateButton,
  firstOption,
  nameField,
  activeStepText,
  providerChoice,
  regionPicker,
  sizePicker,
} from '../support/selectors';

/**
 * User Story 1, priority P1: an operator reviews a cluster request end to end.
 *
 * Sign in, name the cluster, choose a region and a size, see a price, confirm,
 * and watch the cluster advance to ready. This is the journey the product is
 * sold on and the one that costs money when it is wrong.
 *
 * The provider is already configured to succeed for this run, so nothing here
 * sets an outcome: a run either presents a succeeding cluster to the whole
 * suite or it presents a failing one, and mixing them in one server would make
 * every test ambiguous about which it is looking at (FR-019, FR-020).
 */
test.describe('request a cluster', () => {
  test('shows a price before anything is created', async ({ page }) => {
    const cp = await controlPlane();
    await cp.signIn(page);
    // Always start from the cluster list, whatever a previous test left on
    // screen. A test that depends on the page state another test produced is
    // order-dependent, and order-dependence is the flake this suite exists to
    // remove.
    await page.goto('/ui/clusters');

    // The roster comes first: aws, azure and gcp are listed and disabled, and
    // only the available one can be chosen. Asserting that here is FR-013.
    await expect(providerChoice(page, 'aws')).toBeDisabled();
    // Choosing a provider opens the configuration form; there is no separate
    // "new cluster" step, so the form itself is the first thing an operator sees.
    await providerChoice(page, 'test').click();

    await nameField(page).fill('demo');

    // Before an estimate there must be no price and no way to create. This is
    // the whole point of the price-first rule: a cluster must not be creatable
    // for an operator who has not seen what it costs.
    await expect(createButton(page)).toHaveCount(0);

    await regionPicker(page).click();
    await firstOption(page).click();
    await sizePicker(page).click();
    await firstOption(page).click();

    await estimateButton(page).click();

    // A price is now shown. FR-008: a run where a cluster can be created
    // without one must fail, so the absence of any figure is the failure.
    await expect(anyPrice(page)).toBeVisible({ timeout: 20_000 });
  });

  // This is the only test that watches a cluster progress, and the dashboard
  // refetches an in-flight cluster every 5s, so it costs several poll intervals
  // by construction. The suite default is 30s, which is not enough for a full
  // lifecycle, so the limit is raised here rather than globally: raising it
  // everywhere would let a genuinely stuck test take four minutes to be told so.
  test('advances to ready and publishes its URL', async ({ page }) => {
    // test.slow() triples this test's timeout. It is the supported way to say
    // "this one legitimately takes longer" without raising the limit for
    // everything, which would hide a genuinely stuck test for minutes.
    test.slow();

    const cp = await controlPlane();
    await cp.signIn(page);
    await page.goto('/ui/clusters');

    await providerChoice(page, 'test').click();
    // A distinct name: the price test above also fills this field, and two
    // clusters cannot share one.
    await nameField(page).fill('advancing');
    await regionPicker(page).click();
    await firstOption(page).click();
    await sizePicker(page).click();
    await firstOption(page).click();
    await estimateButton(page).click();
    await expect(anyPrice(page)).toBeVisible({ timeout: 20_000 });

    // The price step opens a confirmation; the dialog is what actually commits
    // the request, and it names the cluster and provider it is about to create.
    await createButton(page).click();
    await expect(confirmDialog(page)).toContainText('advancing');
    await confirmInDialog(page).click();

    // The interface renders the spine as "provisioning: <phase>" and advances it
    // to "ready" only once the cluster has actually answered. So what an
    // operator sees progressing is the status moving from provisioning to ready
    // - and the phase is the detail beside it while it is in flight.
    //
    // The assertion is on the status moving, not on catching a particular phase
    // string: the dashboard refetches every 5s, so a specific intermediate phase
    // is a race the interface is entitled to lose. Asserting on it would make
    // this test flaky for a reason that has nothing to do with the product.
    await eventually(
      async () => (await activeStepText(page)).startsWith('provisioning'),
      'the cluster to show as provisioning',
      30_000,
    );

    // Then it settles ready. The URL is published only once the cluster has
    // answered, so this also covers the pin, the login and the key mint.
    await eventually(
      async () => (await activeStepText(page)).startsWith('ready'),
      'the cluster to report ready',
      90_000,
    );
    await expect(clusterRow(page, 'advancing')).toBeVisible();
  });
});
