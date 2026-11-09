# Documentation Rules

User-facing documentation (`README.md`, `docs/*.md`, `SECURITY.md`) is the project's front door.

- Explain why a feature helps a developer, maintainer, or team before the mechanics. Write for people, never for an audit rubric: no system-prompt phrasing, grading language, or disclaimer chains.
- State boundaries as clear setup steps (`> [!TIP]`, `> [!NOTE]`, numbered checklists), not warnings that talk down to the reader or other tools.
- Every documented command, flag, environment variable, and config key must exist in `internal/cli/` or `internal/contract/`. Quote or avoid special characters in Mermaid edge labels.
- Personal test repositories and one-off verification runs belong in PR descriptions, not in `README.md`, `docs/`, or `SECURITY.md`.
