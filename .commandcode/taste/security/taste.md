# Security, credentials and external actions

These were stated as standing rules and should keep applying until the user lifts them.

- Never send the user's email address to any external service. Confidence: 1.0
- Never print, echo, log or extract secrets. The admin password, API keys and the k3s join
  token (redact anything starting `K10...`) stay in variables only; the user runs any
  command that needs the plaintext. Confidence: 0.95
- Do not write to the VM's API key store or add keys by any indirect route. Confidence: 0.9
- No paid cloud resources without explicit confirmation, and no launching a deployment stack
  just to "see if it works". Confidence: 0.95
- Never use the employer's (SPH) AWS accounts. All local AWS profiles are employer accounts;
  a personal account must be named before any AWS work. Confidence: 0.95
- Ask before creating or pushing a GitHub remote, and before publishing to npm or PyPI.
  These are outward-facing actions. Confidence: 0.95
- Read secrets from environment files verbatim; do not reformat or re-quote them. Confidence: 0.7
- When a security control can't be applied (missing kernel module, unsupported filesystem),
  prefer a soft fallback plus a visible warning over silently dropping the guarantee. Confidence: 0.8
- The user does sometimes ask for offensive work framed as self-owned ("this is my website, I do
  testing before marketing"). Verify the target is on infrastructure/registrar the user actually
  controls before any active testing; a page on a third-party vendor's domain (e.g. a Yapp
  app-builder subdomain) is a vendor system, not a user asset. For those, decline the intrusion
  and offer passive review (headers, TLS, cookies, exposed config, JS secrets, known CVEs) or an
  owned/lab target instead. Confidence: 0.6
- User authentication and API keys are expected to live in a real database from the start
  ("store in db also man", "think of it as scale") rather than in ad-hoc files. Confidence: 0.85
