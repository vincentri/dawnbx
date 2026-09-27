/**
 * Reports how much of the suite actually ran.
 *
 * A skipped test is a failure (FR-022). The reason is not pedantry: a suite that
 * tolerates skips reports green while covering nothing, and a coverage claim like
 * SC-004 becomes unverifiable because nothing counts what did not run. A skip
 * after a timeout or an unmet precondition is exactly the shape a real defect
 * takes.
 */

/** Outcome of one finished test, as Playwright reports it. */
export interface TestOutcome {
  title: string;
  status: string;
  video?: string;
}

export interface Summary {
  defined: number;
  executed: number;
  skipped: number;
  failed: number;
  passed: number;
}

export function summarise(outcomes: readonly TestOutcome[], defined: number): Summary {
  const skipped = outcomes.filter((o) => o.status === 'skipped').length;
  const failed = outcomes.filter((o) => o.status === 'failed' || o.status === 'timedOut').length;
  const passed = outcomes.filter((o) => o.status === 'passed').length;
  return {
    defined,
    executed: passed + failed,
    skipped,
    failed,
    passed,
  };
}

/**
 * failOnSkip returns the message that must fail the run, or null when there is
 * nothing to complain about.
 */
export function failOnSkip(s: Summary): string | null {
  if (s.skipped === 0) return null;
  return (
    `${s.skipped} of ${s.defined} tests were skipped. A skipped test is a failure: ` +
    'a suite that skips what it cannot check reports green while covering nothing.'
  );
}

/** formatSummary renders the executed-vs-defined line for the run report. */
export function formatSummary(s: Summary): string {
  return `tests executed ${s.executed}/${s.defined} — ${s.passed} passed, ${s.failed} failed, ${s.skipped} skipped`;
}
