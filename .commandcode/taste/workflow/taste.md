# Workflow preferences

- Wants a plan/design phase before implementation: leans on the gstack review skills
  (/office-hours, /plan-ceo-review, /plan-eng-review, /plan-devex-review, /devex-review) and
  accepts a written design doc plus a decision ledger as the source of truth. Confidence: 0.85
- Answers decision briefs with a single bare letter ("A", "b", "C"), often many in a row.
  Don't pad the brief, and don't ask a second question when one letter is enough. Confidence: 0.9
- Verification before commitment. Repeatedly: "verify first", "prove it before we mark",
  "before agree, can we check first", "test all bro make sure all good an work".
  A decision is only "approved" after a live probe on the real system. Confidence: 0.95
- Expects the agent to run the tests, not to describe them: "you can test for me man". Confidence: 0.8
- Prefers to be asked once, then left alone: "go ahead", "go", "continue" mean execute the
  listed plan end to end without check-ins. Confidence: 0.8
- Strictly local-first sequencing. Do not bring up git hosting, package publishing, release
  artifacts, or cloud deployment until the user raises them ("this 2 later we still focus on
  development"). Confidence: 0.9
- Commit only when asked; when committing, include the attribution line
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Nothing is ever pushed. Confidence: 0.9
- Wants TODO.md / backlog files maintained as decisions are deferred, with dates and reasons,
  rather than dropping scope silently. Confidence: 0.75
- Rejects churn from external churn: a stale document or a renamed project is fine, but prefer
  fixing the plan artifact over re-litigating an old decision. Confidence: 0.6
- When a tool or model is failing, find a different path rather than retrying the same one
  ("then find other way for second opiion", "find other way for second opiion"). Confidence: 0.85
