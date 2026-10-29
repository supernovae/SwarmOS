# Default Repository Rules

- Follow role-owned focused checks and independent review/reuse responsibilities in `.swarm/active/TEST_PLAN.md`. `.swarm/validation/required-checks.yaml` is a coordinator-owned reference, not a per-role execution list. The coordinator runs the complete trusted-base required policy once after roles finish; agent results remain advisory and cannot satisfy claim-bound validation or publication. Repeat commands only for relevant source changes, observed failures or an unresolved coverage concern.
- Keep each WorkUnit focused on its declared objective and file touchpoints so parallel worktrees merge cleanly.
- Do not modify files listed under `protected_paths` in `.swarm/validation/required-checks.yaml` unless explicitly authorized by the WorkUnit.
- Ensure every acceptance criterion in `.swarm/active/WORKUNIT.md` is covered by automated tests or reproducible verification steps in `.swarm/active/TEST_PLAN.md`.
- State what you know with evidence (a file, a test, or command output). Label guesses as assumptions and check them before building on them.
