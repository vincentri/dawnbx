/**
 * Maps a declared outcome to the environment the test provider reads at startup.
 *
 * The provider is built once, when the server starts, so an outcome is a property
 * of a *run* rather than of a test inside it. Tests that need a different outcome
 * therefore run in their own pass, and the validation here is what stops a
 * mis-declared outcome from quietly producing a vacuous test (FR-019, FR-020).
 */

export type ClusterOutcome = 'succeed' | 'fail' | 'unreachable';

export interface TestOutcome {
  cluster: ClusterOutcome;
  /** Required when cluster is 'fail'. A missing reason makes the test prove nothing. */
  failureReason?: string;
  /** An unavailable provider is listed in the roster and cannot be selected. */
  providerAvailable?: boolean;
  /** Removal is refused, as it is for a node still holding sandboxes. */
  holdWorkers?: boolean;
  /**
   * How long a phase lasts, in milliseconds. Deliberately under the dashboard's
   * 5s refetch so a phase is present when the interface next polls; at or above
   * the poll interval every phase observation becomes a race (research.md R-003).
   */
  advanceMs?: number;
}

/** The environment a server must be started with to produce this outcome. */
export function envFor(outcome: TestOutcome): Record<string, string> {
  validate(outcome);
  const env: Record<string, string> = {
    DAWNBX_E2E_CLUSTER: outcome.cluster,
    DAWNBX_E2E_ADVANCE_MS: String(outcome.advanceMs ?? 2500),
  };
  if (outcome.failureReason) env.DAWNBX_E2E_FAILURE_REASON = outcome.failureReason;
  env.DAWNBX_E2E_PROVIDER_UNAVAILABLE = outcome.providerAvailable === false ? '1' : '0';
  env.DAWNBX_E2E_HOLD_WORKERS = outcome.holdWorkers ? '1' : '0';
  return env;
}

/** validate refuses an outcome that would make its test pass for the wrong reason. */
export function validate(outcome: TestOutcome): void {
  if (!['succeed', 'fail', 'unreachable'].includes(outcome.cluster)) {
    throw new Error(`unknown cluster outcome "${outcome.cluster}"`);
  }
  // FR-010 requires the interface to show a specific reason. A failure with no
  // reason would let that test pass without proving a reason is ever rendered.
  if (outcome.cluster === 'fail' && !outcome.failureReason?.trim()) {
    throw new Error('a "fail" outcome needs a failureReason, or the test proves nothing');
  }
  const advance = outcome.advanceMs ?? 2500;
  if (advance <= 0) {
    throw new Error('advanceMs must be positive; a zero would make progression unobservable');
  }
  if (advance >= 5000) {
    throw new Error(
      `advanceMs ${advance} is at or above the dashboard's 5s refetch, which makes every ` +
        'phase observation a race rather than a wait',
    );
  }
}
