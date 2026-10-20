# SwarmOS Architecture

SwarmOS is designed around a single core principle: **Git and GitHub Issues already provide every primitive needed to coordinate distributed software agents—if used with strict schemas and optimistic concurrency.**

---

## 1. Contract Layer (`internal/contract/`)

Every SwarmOS task begins as a `WorkUnit` validated against [`workunit.schema.json`](../.swarm/contracts/workunit.schema.json).

`ParseWorkUnitFromIssue` supports three deterministic formats in priority order:
1. **Fenced YAML Block** (` ```yaml swarm-workunit ` or ` ```yaml `).
2. **GitHub Issue Form Sections** (`### Objective`, `### Constraints`, `### Acceptance Criteria`, `### Workflow`, `### Plan Gate`).
3. **External File Reference** (`swarm-workunit-path: .swarm/workunits/<id>.yaml`).

### Prompt Injection Defense
Before parsing any issue body:
- `StripHTMLComments` removes all `<!-- ... -->` blocks so hidden instructions inside untrusted issue descriptions never reach agent system prompts.
- `VerifyTrustPolicy` in `internal/lease/manager.go` verifies that the issue either carries the maintainer-controlled `swarm:ready` label or was authored by a user whose `AuthorAssociation` is in `security.trusted_associations` (`OWNER`, `MEMBER`, `COLLABORATOR`). Untrusted issues fail immediately with `ErrUntrustedIssue` before any worktree or agent process is spawned.

---

## 2. Serverless Lease & Artifact Protocol (`internal/lease/`)

SwarmOS stores orchestration state inside a single living comment per claim on the issue using an embedded HTML state header followed by rendered Markdown tables and collapsible `<details>` sections:

```markdown
<!-- swarm:state:v1 {"lease":{...},"artifacts":[...]} -->
### SwarmOS Orchestration State
...
```

### Optimistic Concurrency & Comment-ID Tie-Breaking
Because GitHub Issues do not offer server-side locks, `Manager.Acquire` implements a deterministic two-phase claim protocol:
1. Fetch existing issue comments ordered by ascending `CommentID`.
2. Parse all `<!-- swarm:state:v1 ... -->` blocks. If an unexpired active lease (`status == "active"` and `expires_at > now`) belongs to another owner, abort with `ErrAlreadyClaimed`.
3. Post our claim comment (or update our existing comment).
4. Re-fetch issue comments and inspect all unexpired active leases. The claimant whose comment has the **lowest `CommentID`** wins. Any concurrent claimant that posted milliseconds later detects that an earlier `CommentID` exists, marks its own comment `superseded`, and returns `ErrClaimRaceLost`.

### Lazy Lease Expiration
If an agent crashes or a developer disconnects without running `swarm release`, the lease automatically becomes reclaimable once `expires_at <= now`. The next claimant seamlessly acquires the issue without manual cleanup.

---

## 3. Worktree Isolation & Universal Harness Compiler (`internal/workspace/`)

When an issue is claimed, `workspace.Provision` creates an isolated Git worktree:
- **Path**: `.swarm/worktrees/issue-<N>`
- **Branch**: `swarm/issue-<N>-<slug>`

`workspace.CompileHarnessBundle` then compiles role-specific context into the worktree:
- `.swarm/active/WORKUNIT.md` — Structured contract and prior phase artifacts.
- `.swarm/active/SYSTEM_PROMPT.md` — Concatenation of `.swarm/prompts/<role>.md`, `.swarm/project.md`, `.swarm/architecture.md`, `.swarm/conventions.md`, and `WORKUNIT.md`.
- Managed pointer blocks (`<!-- swarm:active:start --> ... <!-- swarm:active:end -->`) in `AGENTS.md` and `CLAUDE.md` so any agent opened interactively inside the worktree immediately discovers the active contract.
- `.swarm/active/` is automatically added to the worktree's `.git/info/exclude`, and `CleanEphemeralHarnessMarkers` strips temporary markers prior to `swarm submit`.

---

## 4. Validation & Tamper Defense (`internal/validation/`)

Before any work unit can be submitted as a Draft PR, `validation.Validate` enforces three checks:
1. **Artifact Completeness**: Verifies `.swarm/active/PLAN.md` (`design`) and `.swarm/active/TEST_PLAN.md` (`test-plan`) exist and are non-empty.
2. **Protected-Path Tamper Detection**: Diffs the worktree against `HEAD`/`origin/HEAD`. If any modified file matches `protected_paths` (`.github/workflows/**`, `.swarm/validation/**`, `.swarm/contracts/**`) and `allow_harness_changes` is `false`, validation fails with `ErrProtectedPathModified`.
3. **Required Checks Execution**: Runs every command in `.swarm/validation/required-checks.yaml` (`build`, `test`, `lint`), capturing exit codes, durations, and output into a `validation` artifact.
