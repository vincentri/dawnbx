import { expect, type Locator, type Page } from '@playwright/test';

/**
 * Waits for an operator-visible state.
 *
 * The rule this encodes: a test waits for a state the interface shows, never for
 * an elapsed duration. The dashboard refetches an in-flight cluster every 5s, so
 * a phase becomes visible on that cadence and not before — a test that slept for
 * a guessed interval would be racing the very timer it cannot control
 * (research.md R-003).
 *
 * A timeout here is a failure and never a pass. That is the difference between a
 * test that checks something and one that waits out its own timeout.
 */

/** How long to wait for a state the UI is expected to reach. */
export const STATE_TIMEOUT = 45_000;

/** delay waits without nesting a callback, per the project's Promise rule. */
function delay(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

/** visible waits for text to appear anywhere on the page. */
export async function visible(page: Page, text: string, timeout = STATE_TIMEOUT): Promise<Locator> {
  const el = page.getByText(text, { exact: false }).first();
  await expect(el, `waiting for "${text}" to be visible`).toBeVisible({ timeout });
  return el;
}

/** gone waits for text to disappear, which is how a phase change is observed. */
export async function gone(page: Page, text: string, timeout = STATE_TIMEOUT): Promise<void> {
  await expect(page.getByText(text, { exact: false }).first(), `waiting for "${text}" to go away`).toBeHidden({
    timeout,
  });
}

/**
 * present waits for a locator to exist without requiring it to be actionable.
 * Used where the interface renders an element it may still be populating.
 */
export async function present(locator: Locator, description: string, timeout = STATE_TIMEOUT): Promise<void> {
  await expect(locator, `waiting for ${description}`).toBeVisible({ timeout });
}

/**
 * eventually retries a probe until it returns true.
 *
 * For a condition that is not a single element — a cluster's status having
 * settled, a price having appeared in a panel — where expressing it as a locator
 * would be more brittle than reading it once.
 */
export async function eventually(
  probe: () => Promise<boolean>,
  description: string,
  timeout = STATE_TIMEOUT,
): Promise<void> {
  const deadline = Date.now() + timeout;
  for (;;) {
    if (await probe()) return;
    if (Date.now() >= deadline) {
      throw new Error(`timed out after ${timeout}ms waiting for ${description}`);
    }
    await delay(250);
  }
}

/** neverWithin asserts a condition stays false for a window, for "must not" rules. */
export async function neverWithin(
  probe: () => Promise<boolean>,
  description: string,
  window: number,
): Promise<void> {
  const deadline = Date.now() + window;
  while (Date.now() < deadline) {
    if (await probe()) throw new Error(`${description}, but it happened`);
    await delay(200);
  }
}
