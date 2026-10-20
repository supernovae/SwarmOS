# Future work

## Optional trusted coordinator / admin CLI

Git compare-and-swap state refs are the implemented and authoritative coordination mechanism. Do not add a second state authority or silently fall back from Git CAS.

A future **optional** trusted coordinator or admin CLI may be considered only if Git-CAS operational evidence shows a need for central scheduling or recovery. It is not implemented.

Required design contract before implementation:

- The coordinator is explicitly enabled per repository and has a distinct, authenticated operator identity; ordinary workers and issue commenters cannot invoke administrative mutations.
- Git CAS remains the source of truth, or the coordinator has a documented, atomic claim-store contract with fencing tokens, durable attempt IDs, revision checks, and an auditable recovery path. There must never be split authority between comments, a database, and refs.
- Authorization distinguishes task approval, human plan approval, state mutation, release/reassignment, and submission. Every mutation is bound to repository, issue, task digest, policy digest, base SHA, claim ID, and current state version.
- The claim store provides compare-and-swap semantics, expiry, renewal, fencing of stale workers, idempotent mutation/submission, and deterministic crash recovery. It must preserve a successor's checkpoint.
- Publishing credentials remain with the trusted coordinator; agent and validation processes receive only the minimum explicitly permitted credentials.
- The CLI exposes read-only inspection and audit output, never an undocumented bypass of repository policy or human gates.

Acceptance evidence: concurrent same-owner and different-owner claims permit one executor; delayed/stale workers cannot publish or submit; approval and validation are bound to the exact task/plan/test/base/head revisions; crash/restart recovers without overwriting a successor; unauthorized users cannot administer state; all state transitions are auditable; and Git-CAS versus coordinator behavior is documented without claiming a feature that does not exist.

## Planned optimization work

Beyond the implemented `--max-jobs` limit, these remain planned rather than guaranteed: wall-time/cost budgets, repository fairness, richer structured event streams, and provider capability diagnostics. Cost limits require provider usage data; elapsed time must not be represented as a cost estimate.
