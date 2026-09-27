import { expect, test } from '@playwright/test';
import { ADMIN_USER, controlPlane } from '../fixtures/control-plane';

/**
 * User Story 1, first step: an operator signs in.
 *
 * The suite authenticates through the real form rather than by injecting a
 * session, so the first flow every operator performs is exercised rather than
 * bypassed (FR-021). The credential is read from the file the control plane
 * itself wrote and never appears in a locator, an assertion, or a message.
 */
test.describe('sign in', () => {
  test('reaches the signed-in shell with the run credential', async ({ page }) => {
    const cp = await controlPlane();

    await cp.signIn(page);

    // The shell is the assertion: the nav is present and the sign-in form is
    // gone. Asserting on the nav rather than on "Sign out" is deliberate — that
    // item lives inside a dropdown menu that is closed until opened, so it is not
    // something a signed-in shell guarantees to be on screen.
    await expect(page.getByRole('navigation')).toBeVisible();
    await expect(page.getByRole('button', { name: /^sign in$/i })).toHaveCount(0);
    // The identity chip only renders for a signed-in operator, so it is the
    // assertion that actually distinguishes the two shells.
    await expect(page.getByText(/admin|member of/i).first()).toBeVisible();
  });

  test('refuses a wrong password rather than appearing to sign in', async ({ page }) => {
    await page.goto('/ui/');
    await page.getByLabel(/username/i).fill(ADMIN_USER);
    await page.getByLabel(/password/i).fill('not-the-password-the-run-generated');
    await page.getByRole('button', { name: /sign in/i }).click();

    // A failed sign-in must leave the operator on the form. A dashboard that
    // renders while signed out would make every other test in this suite
    // meaningless, because none of them would be proving anything.
    await expect(page.getByRole('button', { name: /sign in/i })).toBeVisible();
  });
});
