---
description: Audit .swarm/active/PLAN.md and TEST_PLAN.md against the active SwarmOS WorkUnit contract
---

Read `.swarm/active/WORKUNIT.md`, `.swarm/active/PLAN.md`, `.swarm/active/TEST_PLAN.md`, and the approved instructions in `.swarm/active/SYSTEM_PROMPT.md`.
Evaluate whether the plan satisfies the Objective, respects all Constraints and protected paths, and covers all Acceptance Criteria. Check every item under **What we know** against evidence you can verify, and apply the compiled approved execution profile when present. Advisory dimension suggestions never select a profile.
Write `.swarm/active/REVIEW.md` starting with `VERDICT: APPROVED` or `VERDICT: CHANGES_REQUESTED` on line 1, followed by your structured findings.

Audit expensive-check ownership, expected duration and named reuse roles. Flag a check whose expected duration exceeds the owning role's available runtime, and a check run in full by more than one role without a stated reason. Use approved runtime information; record unknown runtime limits as open questions rather than inventing timeouts.

## Structured plan findings

For initial-issue agent plan reviews, keep the first-line VERDICT format and add exactly one JSON block:

<!-- swarm:plan-review-findings:start -->
{"version":1,"findings":[]}
<!-- swarm:plan-review-findings:end -->

APPROVED requires zero open blocking findings; CHANGES_REQUESTED requires at least one. A finding is blocking when, if the plan is implemented as written, an acceptance criterion, constraint or invariant would not be met or not be proven. Do not approve while asking the developer or tester to handle such a gap; the tester will mark the criterion unmet after implementation and the full validation cycle is wasted. Non-blocking findings are only for improvements that do not affect whether any criterion is met. Each finding has `id` (F1, F2, ...), `blocking` (boolean), and `disposition` (`open`, `resolved`, `withdrawn`, or `superseded`). Every blocking entry, including closed entries, requires concrete nonblank `criterion`, `trigger`, `expected`, `actual`, and `check` fields. Cite the violated acceptance criterion or invariant; a reachable source-level counterexample is a valid trigger before code exists. Name the smallest distinguishing check.

Example blocking entry: `{"id":"F1","blocking":true,"disposition":"open","criterion":"Claim fencing invariant","trigger":"A stale claim calls UpdateArtifact","expected":"ErrClaimFence","actual":"The proposed write bypasses the claim check","check":"Unit test a stale claim artifact write"}`.

Read REVIEW_EVIDENCE.md and carry every prior ID forward with its current disposition. Allocate new IDs above every prior ID; never recycle an ID or change its criterion. A superseded record requires `superseded_by` naming an existing different ID; replacement chains must not cycle. Reopening a resolved entry requires `reversal_note` citing new evidence. Do not omit withdrawn or superseded records. Entries may contain only `id`, `blocking`, `disposition`, `criterion`, `trigger`, `expected`, `actual`, `check`, `superseded_by`, and `reversal_note`. Any other key, such as `note` or `comment`, makes the whole review invalid. A non-blocking entry uses the same keys and puts its advice in `criterion` and `check`, for example `{"id":"F2","blocking":false,"disposition":"open","criterion":"Test readability","check":"Name the table cases after the scenario"}`. Use no duplicate keys or null values, and keep the block within 64 KiB. Free-form notes go in prose outside the block and never prevent approval.

Planners receive only open blocking findings and must respond per ID with their edit and verification, or a reasoned counterexample. Independently determine dispositions. Human review-plan decisions and PR revision reviews retain their existing protocol; agent verdicts never satisfy a human gate.
