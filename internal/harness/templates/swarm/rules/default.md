# Default Repository Rules

- Run the checks `.swarm/active/TEST_PLAN.md` assigns to your role and reuse recorded results that still apply. `.swarm/validation/required-checks.yaml` is a coordinator-owned reference, not a per-role execution list.
- Keep each WorkUnit focused on its declared objective and file touchpoints so parallel worktrees merge cleanly.
- Do not modify files listed under `protected_paths` in `.swarm/validation/required-checks.yaml` unless explicitly authorized by the WorkUnit.
- Ensure every acceptance criterion in `.swarm/active/WORKUNIT.md` is covered by automated tests or reproducible verification steps in `.swarm/active/TEST_PLAN.md`.
- State what you know with evidence (a file, a test, or command output). Label guesses as assumptions and check them before building on them.
