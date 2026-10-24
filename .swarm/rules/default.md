# Default Repository Rules

- Follow the build, test, and lint commands defined in `.swarm/validation/required-checks.yaml`.
- Keep each WorkUnit focused on its declared objective and file touchpoints so parallel worktrees merge cleanly.
- Do not modify files listed under `protected_paths` in `.swarm/validation/required-checks.yaml` unless explicitly authorized by the WorkUnit.
- Ensure every acceptance criterion in `.swarm/active/WORKUNIT.md` is covered by automated tests or reproducible verification steps in `.swarm/active/TEST_PLAN.md`.
- State what you know with evidence (a file, a test, or command output). Label guesses as assumptions and check them before building on them.
