import type { Locator, Page } from '@playwright/test';

/**
 * Selectors for the cluster request flow.
 *
 * They are gathered here rather than written inline so a markup change breaks in
 * one file, and so a test reads as the journey an operator takes rather than as
 * a pile of selectors. Each is anchored to something a user can see: a label, a
 * button name, or the text the interface actually renders.
 */

/**
 * Chooses a provider. The cluster page opens on a provider chooser rather than a
 * form, because the roster is what tells an operator what exists at all: aws,
 * azure and gcp are listed and disabled, and only an available one can be picked.
 */
export const providerChoice = (page: Page, id: string): Locator =>
  page.getByRole('button', { name: new RegExp(`^${id}(\\s|$)`) });

/** Opens the request form. It only exists after a provider is chosen. */
export const newClusterButton = (page: Page): Locator => page.getByRole('button', { name: 'New cluster' });

/**
 * The cluster's name field. The accessible name is "cluster name", not the
 * "Name" label the field is visually captioned with, because the input carries
 * its own aria-label.
 */
export const nameField = (page: Page): Locator => page.getByRole('textbox', { name: 'cluster name' });

/** The region picker. A combobox in the real markup, not a select element. */
export const regionPicker = (page: Page): Locator => page.getByRole('combobox', { name: 'region' });

/** The instance size picker. */
export const sizePicker = (page: Page): Locator => page.getByRole('combobox', { name: 'size' });

/** Picks one option from an open combobox. */
export const firstOption = (page: Page) => page.getByRole('option').first();

/** Asks for a price. Nothing is created by this. */
export const estimateButton = (page: Page): Locator => page.getByRole('button', { name: 'Estimate price' });

/** The price panel, present once a quote exists. */
export const pricePanel = (page: Page): Locator => page.getByText('Price', { exact: false }).first();

/** A monthly figure anywhere on the page. The price is always shown per month. */
export const anyPrice = (page: Page): Locator => page.getByText(/\/\s*mo|\$[0-9]/).first();

/**
 * Opens the confirmation from the price step. This is the step's own button;
 * the request is not committed until confirmInDialog is used.
 */
export const createButton = (page: Page): Locator => page.getByRole('button', { name: 'Confirm and create' });

/**
 * Commits the request inside the confirmation dialog. Scoped to the dialog
 * because the price step behind it also has a "Confirm and create" button, and
 * an unscoped "Create cluster" matches the dialog's heading as well as its
 * action.
 */
export const confirmInDialog = (page: Page): Locator =>
  page.getByRole('dialog').getByRole('button', { name: 'Create cluster' });

/** The dialog's title, which names what is about to be created. */
export const confirmDialog = (page: Page): Locator => page.getByRole('dialog');

/** Returns to the configuration step to change what is being priced. */
export const changeConfigurationButton = (page: Page): Locator =>
  page.getByRole('button', { name: 'Change configuration' });

/**
 * The ACTIVE step on the provisioning spine.
 *
 * The spine always renders every step, and marks the current one with
 * `font-medium`; the active step also carries the phase, so its text is
 * "ready: verifying" rather than "ready". Asserting on either the raw string or
 * a class would have passed against a cluster still provisioning - the raw
 * string matches the inactive step that is always on screen, and the class
 * matches every step at once. Reading the text and requiring the phase suffix is
 * what makes this the step the cluster is actually on.
 */
export function activeStep(page: Page): Locator {
  return page.locator('li.font-medium');
}

/** The active step's text, trimmed. Empty when nothing is active. */
export async function activeStepText(page: Page): Promise<string> {
  return ((await activeStep(page).first().textContent()) ?? '').trim();
}

/**
 * A phase badge, matched EXACTLY.
 *
 * This is not a style preference. With exact: false, a search for "ready" also
 * matches the Ready badge on the settings page and any sentence containing the
 * word, so a test asserting a cluster reached ready passed in 1.1 seconds
 * against a cluster still sitting in requesting_host. A phase is a whole word
 * with an exact value, and it is matched as one.
 */
export const phaseBadge = (page: Page, phase: string): Locator =>
  page.getByText(phase, { exact: true }).first();

/**
 * A cluster's row in the sidebar.
 *
 * Scoped to the sidebar, not the whole page, because a deleted cluster's name
 * stays in the detail panel's heading after it leaves the list: an assertion that
 * searched the page found the heading and concluded the delete had not worked,
 * while the sidebar had already dropped the row. The row is what the assertion
 * is about.
 */
export function clusterRow(page: Page, name: string): Locator {
  // The cluster list is a Card, not a nav or an aside, so it is found by the
  // heading it carries. Scoping to it is what makes "the row left the list" a
  // statement about the list.
  return page
    .locator('div', { has: page.getByText('Every cluster this control plane manages.') })
    .locator('button', { hasText: name })
    .first();
}

/** The detail panel's failure reason, shown when a cluster will not come up. */
export const failureDetail = (page: Page): Locator => page.getByText(/failed|could not|unreachable|refus/i).first();

/** The refusal notice for an operation the product will not perform. */
export const refusalNotice = (page: Page): Locator => page.getByText(/still holds sandboxes|worker|node/i).first();

/**
 * Opens the worker section of a cluster's detail panel. The add-worker control
 * does not exist until this is open, so a test that reaches for "Add worker"
 * first is looking for a button that has not been rendered.
 */
export const workerNodesButton = (page: Page): Locator =>
  page.getByRole('button', { name: 'Worker nodes' });

/**
 * The worker's size picker, inside the worker section. It is labelled "worker
 * size", distinct from the cluster's own "size", and "Add worker" stays disabled
 * until it has a value.
 */
export const workerSizePicker = (page: Page): Locator =>
  page.getByRole('combobox', { name: 'worker size' });

/** Returns from the worker section to the cluster detail. */
export const backToClusterButton = (page: Page): Locator =>
  page.getByRole('button', { name: 'Back to the cluster' });

/** The add-worker control inside the worker section. */
export const addWorkerButton = (page: Page): Locator => page.getByRole('button', { name: 'Add worker' });

/** Deletes the cluster from its detail panel. */
export const deleteClusterButton = (page: Page): Locator =>
  page.getByRole('button', { name: 'Delete cluster' });
