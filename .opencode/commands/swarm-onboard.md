---
description: Onboard this repository to SwarmOS, or upgrade its SwarmOS files after installing a newer swarm, without disturbing what the team already has
---

You are helping a maintainer set up SwarmOS in their repository, or bring an existing setup up to date with a newer `swarm`. `swarm repo init` writes the files; your job is to make them fit this repository: real validation checks, accurate project notes, and careful merges with anything the team already has. Work with the maintainer, show them what you find, and ask before you write.

## 1. Get to know the repository

1. Read the existing agent guidance (`AGENTS.md`, `CLAUDE.md`, and similar files), the CI workflows under `.github/workflows/`, and the build entry points (`Makefile`, `package.json` scripts, `pyproject.toml`, `Cargo.toml`, `go.mod`, and so on).
2. Note the layout: a single project or a monorepo, which languages, and where the tests live.
3. Run `swarm repo doctor` and share anything it reports as missing (Git, GitHub access, agent harnesses).

## 2. Preview what init would change

Run:

```bash
swarm repo init --dry-run --json
```

Nothing is written. The output sorts every SwarmOS file into one of these groups:

- `created`: new files.
- `updated`: files still exactly as an earlier SwarmOS generated them, which will be refreshed.
- `customized`: SwarmOS files the team has since edited. They are kept as is.
- `preserved`: files at SwarmOS paths that the team created themselves. They are kept as is.
- `sections`: repository files such as `AGENTS.md` that get a marked SwarmOS section. Only the text between the `swarm:harness` markers is managed.
- `removed`: files an earlier SwarmOS generated that this version no longer owns.

`diffs` shows, for each existing file, what changes between its current content and what this version generates. Summarize the preview for the maintainer and call out anything that needs a decision. For example, an existing issue template or `.claude/commands/` file at a SwarmOS path, or a CI workflow already named `swarm-validate.yml`.

## 3. Agree on the validation checks

Every WorkUnit is validated against `.swarm/validation/required-checks.yaml`, so this is the file that matters most. Init guesses build, test, and lint commands from the detected stack. In a larger repository those guesses are often incomplete.

1. Propose `required_checks` entries (`name`, `command`, `timeout`) based on what CI actually runs.
2. Prefer commands contributors can run locally in a clean checkout. Point out suites that are slow, flaky, or need services or secrets, and suggest a scoped command or a longer timeout instead.
3. Keep the default `protected_paths` and add any paths the team considers sensitive. Don't remove defaults unless the maintainer asks you to.
4. Wait for the maintainer to agree before writing anything.

## 4. Run init and tailor the files

1. Run `swarm repo init`. Add `--sync-labels` only if the maintainer wants the `swarm:*` labels created on GitHub now, and `--fork-coordinator` only if they plan to accept contributions from forks.
2. Apply the agreed `required-checks.yaml`.
3. Fill in `.swarm/project.md`, `.swarm/architecture.md`, and `.swarm/conventions.md` from what you learned in step 1. Keep them short and factual: these are what agents read before planning work.
4. Draft `.swarm/goals.md` with the maintainer. Ask what they want contributors and agents to focus on.
5. If the team uses a harness other than the one set as `default_agent` in `.swarm/config.yaml`, ask whether to change it.

## 5. Reconcile kept files

For each `customized` or `preserved` file, use its diff from step 2 to propose a merge that keeps the team's intent and picks up what this version adds. Show the merged result and apply it only once the maintainer agrees. In `AGENTS.md` and `CLAUDE.md`, never change anything outside the SwarmOS markers.

## 6. Check the result

1. Run `swarm repo doctor` again.
2. Run each command in `required-checks.yaml` once and share the results. Fix or rescope any check that fails before going further.
3. Run `swarm repo init --dry-run` once more. Your edits now show as customized files, which is expected.

## 7. Hand it back

Show `git status` and `git diff`, then suggest committing on a new branch and opening a pull request so the team reviews the SwarmOS setup like any other change. Don't commit or push unless the maintainer asks you to.

## Upgrading later

After installing a newer `swarm`, run this guide again. Steps 2, 5, and 6 cover most upgrades: preview the changes, merge anything the team customized, and confirm the checks still pass.
