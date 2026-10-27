# Project & Maintainer Setup Guide

This guide walks repository maintainers through initializing SwarmOS, configuring `.swarm/config.yaml` and validation rules, authoring and approving `WorkUnit` issues, and setting up GitHub repository protections.

---

## 1. Initialize and Commit the Harness

Install a resource-CLI-compatible `swarm` release from [**GitHub Releases**](https://github.com/supernovae/SwarmOS/releases/latest) (or `go install github.com/supernovae/swarmos/cmd/swarm@<compatible-release>`). Until one is published, build this source checkout with `go build -o swarm ./cmd/swarm` and use that binary. Then run `swarm repo init` inside your repository checkout:

```bash
swarm repo init --sync-labels --backend github
swarm repo doctor --backend github
```

`swarm repo init` detects your project stack (`Go`, `Rust`, `Python`, or `TypeScript/JavaScript`) and scaffolds:

| Path | Role |
| :--- | :--- |
| `.swarm/config.yaml` | Default agent, `plan_mode` (`auto\|agent\|interactive`), plan review gate, lease TTL/renewal limits, role routing, collaborator allowlists, and allowed agent harnesses. |
| `.swarm/goals.md`, `SKILLS.md`, `rules/`, `skills/` | Project mission, safety invariants, agent factory staging guide, and reusable rules/skills compiled into `SYSTEM_PROMPT.md`. |
| `.swarm/project.md`, `architecture.md`, `conventions.md` | Project context, build/test commands, and conventions compiled into agent prompts. |
| `.swarm/workflows/{feature,bugfix,refactor}.yaml` | Role prompt recipes for feature delivery, bug fixes, and refactors. |
| `.swarm/prompts/{planner,reviewer,developer,tester}.md` | Role contracts defining what each phase reads and produces. |
| `.swarm/contracts/{workunit,artifact}.schema.json` | JSON Schemas (Draft 2020-12) for validating issues and artifacts. |
| `.swarm/validation/required-checks.yaml` | Required check commands and protected-path globs enforced before submission. |
| `AGENTS.md` | Agent quick reference. If your repository already has an `AGENTS.md`, init appends a short SwarmOS section between `<!-- swarm:harness:start -->` and `<!-- swarm:harness:end -->` markers and only ever rewrites that section. If `AGENTS.md` or `CLAUDE.md` is a symlink, for example `AGENTS.md -> CLAUDE.md`, init leaves the link in place and writes the section once into the file it points to. The link must be relative and lead to a regular file inside the repository; links that leave the repository (even briefly), point into `.git/` or `.swarm/`, or lead to a directory, special file, or missing file stop init before it writes anything. Task worktrees follow the same rule, so a linked pair gets a single temporary task pointer. |
| `.github/ISSUE_TEMPLATE/{swarm-workunit,swarm-community,swarm-feature,swarm-bugfix,swarm-refactor}.yml` | Preset GitHub Issue Forms for structured draft WorkUnits (`swarm:draft`) and community bug/idea intake (`swarm:community`). Your issue chooser settings (`config.yml`) stay yours. |
| `.github/workflows/swarm-validate.yml` | Consumer CI workflow running your repository checks against the trusted base commit. It is a separate workflow file; init never edits your other workflows. |
| `.claude/commands/swarm-*.md` & `.opencode/commands/swarm-*.md` | `/swarm-plan`, `/swarm-review`, `/swarm-implement`, `/swarm-validate`, `/swarm-project-plan`, and `/swarm-onboard` slash commands. |

> [!TIP]
> - `swarm repo init` pins generated automation to the swarm you run it with (a `v0.6.0`+ release, or the exact commit of a clean source build); a build with uncommitted changes keeps your existing pin. Rerun it after upgrading swarm to update untouched generated files; files you edited, and existing files SwarmOS did not generate, are kept and listed. `.swarm/managed.json` records what init generated. Commit the reviewed `.swarm/`, `.github/`, `.claude/`, and `.opencode/` files to your default branch before approving work.
> - `swarm repo init --sync-labels --backend github` provisions local issue forms and `swarm:*` labels on the repository; it does not configure organization-wide GitHub issue types or restrict direct GitHub web/API issue creation. Enable the organization `Epic` issue type in GitHub Settings, review your repository's issue forms and maintainer permissions manually, and run `swarm repo doctor --backend github` to verify that `Epic` is enabled and your current actor is authorized for `create-epic`.
>
> When upgrading, install a resource-CLI-compatible binary and review the generated workflow, slash-command instructions, and immutable pin together. Existing generated files are preserved by default; update the relevant tracked copies deliberately rather than using blanket `--force` scaffolding.
---

## Consumer CI and release administration

With a compatible installed release or explicit compatible pin, `swarm repo init` generates `.github/workflows/swarm-validate.yml` configured for your repository's default branch and language toolchain (`Go`, `Node 24`, `Python 3.13` with `pytest`/`ruff`, or `Rust` with `clippy`), then runs `swarm repo validate --base "$BASE_SHA"` against the pull request or push base SHA. This checkout-only CI command needs a trusted full base commit but no issue claim or task artifacts.

Maintainers can change protected policy in their own pull requests without the check getting in the way. When the author has write, maintain, or admin access and the branch lives in your repository, the workflow runs `swarm repo validate --protected-paths report`: it lists each changed protected file in the job summary, notes when `.swarm/` policy changed (approved WorkUnits then need `swarm issue approve` again after merge), and still runs every required check. Agent branches (`swarm/...`), fork pull requests, and other authors keep strict enforcement. The one exception is an agent pull request for a WorkUnit you approved with `allow_harness_changes: true`. When the head branch `swarm/issue-<N>-...` lives in your repository, a step that runs only the trusted validator reads issue #N's authoritative swarm state. It accepts the protected changes only when that state still approves the unchanged WorkUnit and its plan gate, records this pull request at exactly the head being checked, and the `.swarm/` policy at the target branch matches the policy approved for the task. Pull request titles, bodies, labels, and comments never grant the exception, and fork pull requests are always enforced. The step that runs your checks gets no token.

For repositories that mirror SwarmOS's own release and security setup:
- **Release workflow (`release.yml`)**: Runs the lint, vulnerability, and Linux/macOS test matrix for the tagged commit (Windows binaries are cross-compiled; Windows tests are paused for now), verifies the semantic version tag is reachable from the default branch, and publishes through the `release` GitHub Environment. Configure required reviewers and tag protection rules on that environment in GitHub Settings.
- **Dependency Review (`security.yml`)**: Requires GitHub's [dependency graph](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/secure-your-dependencies/enable-dependency-graph) to be enabled under **Settings → Advanced Security** (along with Dependabot alerts).

---

## 2. Configure Approved Agents (`.swarm/config.yaml`)

Start with one Codex login for all four roles on a supported Linux worker:

```yaml
version: 1
default_agent: codex
default_gate: agent
execution:
  mode: isolated
  backend: native
lease:
  duration: 4h
  renewable: true
  max_renewals: 3
  validation_repair_limit: 1
security:
  require_ready_label: true
  ready_label: swarm:ready
  require_isolation: true
agents:
  codex:
    binary: codex
    roles: [planner, reviewer, developer, tester]
```

Sign in through the selected harness on the worker, then run `swarm repo doctor`. Isolated adapters use coordinator-selected permissions; agent entries can choose a model and roles, while arbitrary `args`, environment forwarding, and sandbox overrides are rejected. For optional offline containers, select `execution.backend: oci` and follow the [OCI setup](../SECURITY.md#optional-oci-backend).

`lease.validation_repair_limit` is optional. It sets how many developer repair rounds `swarm issue work` and `swarm pr work` may start after the trusted checks fail: omission gives one round, and zero disables automatic repair. See [Repair brief for failed validation](developer.md#repair-brief-for-failed-validation).

How agent configuration works:
- **Explicit allowlist**: Only entries under `agents:` can be selected. Each entry can configure `binary`, `harness` (`claude`, `codex`, `omp`, or `opencode`; defaults to the entry name), `args`, `model`, allowed `roles`, permitted `env` variables, and reviewer sandbox settings.
- **Precedence**: Role-specific CLI flag (`--planner-agent`, `--reviewer-agent`, `--developer-agent`, `--tester-agent`) $\rightarrow$ generic `--agent` $\rightarrow$ `roles.<role>` $\rightarrow$ `default_agent`.
- **Maintainer-controlled approvals**: When `security.require_ready_label` is `true`, an issue must be approved by a user with repository write/maintain/admin permissions via `swarm issue approve <issue>` (or `swarm issue create --ready`) before any worker can claim it.

### Agent run budgets

Give slower workers more time through approved execution policy:

```yaml
execution:
  mode: isolated
  backend: native
  agent_timeout: 90m
  role_timeouts:
    developer: 2h
    reviewer: 45m
```

Both settings accept positive Go durations such as `30m` or `2h30m`. The optional `role_timeouts` map accepts `planner`, `reviewer`, `developer`, and `tester`; a role override takes precedence over `agent_timeout`. Omitted settings give unattended agents 60 minutes. Invalid settings fail `swarm repo doctor` and `swarm repo validate`.

Native Codex and harness-tools Bubblewrap interactive sessions count cumulative agent working time, excluding coordinator prompts waiting for your next instruction. A separate four-hour wall cap continues during input waits. Interactive OCI sessions use a four-hour wall cap without idle exclusion; an explicit timeout shortens that cap when smaller. Unattended OCI uses the configured timeout or the 60-minute default.

Run `swarm repo doctor` to see each role's budgets for your local configuration. Actual runs load policy from the approved base commit; editing a candidate checkout cannot change it. Coordinator limits can shorten these budgets. Required-check deadlines and individual tool-call deadlines remain separate.

SwarmOS warns once shortly before a deadline, naming the role and remaining time. The warning lead is two minutes, or ten percent of the budget when shorter. Interactive warnings appear in the terminal. For manual recovery after a timeout, see [Recovering from an Interrupted Run](developer.md#recovering-from-an-interrupted-run).

### Shared defaults and personal choices

Keep team defaults in the project root's `.swarm/config.yaml`: agent presets under `agents`, role assignments under `roles`, and the worker environment under `execution`. Commit changes before approving tasks; existing tasks need fresh approval to adopt updated policy. Set `security.require_isolation: true` to require isolated workers.

For teams planning in an existing agent session, set `plan_mode: interactive` so tasks can be claimed without launching a planner. Write the plan in the assigned worktree and record it with `swarm issue stage <issue> --phase plan`.

Developers choose from those presets with `--agent` or a role flag such as `--developer-agent`. Use `--headless=false` for pairing, or the default headless mode for automation. `SWARM_EXECUTION_MODE` selects a mode for your shell or CI job within project policy. The user-level `~/.config/swarm/node.yaml` describes worker capabilities for task matching; agent permissions belong in the project presets.

For a `trusted-local` Codex setup, for example, add named presets to your existing `agents` map:

```yaml
agents:
  codex-pair:
    harness: codex
    binary: codex
    roles: [developer, tester]
    args: ["--sandbox", "workspace-write", "--ask-for-approval", "on-request"]
  codex-worker:
    harness: codex
    binary: codex
    roles: [developer, tester]
    args: ["--sandbox", "workspace-write", "-c", 'approval_policy="never"']
```

Choose `--developer-agent codex-pair --headless=false` to pair, or `--developer-agent codex-worker` for unattended work. The worker preset keeps Codex's workspace sandbox enabled and turns off approval prompts. SwarmOS places `args` before the driver's subcommand, so use options accepted in that position. See the [Codex CLI reference](https://developers.openai.com/codex/cli/reference) for permission options.

Provider authentication belongs to the harness. The current `trusted-local` launcher accepts model credential variables listed in `agents.<name>.env`; keep their values in your shell or CI secret store. Isolated adapters use harness-owned login directly, without those environment presets. Leave `model` unset for harness selection, or declare the project's model choice in its agent entry. The [worker guide](developer.md#choose-where-agents-run) explains the available setup paths.

### Checkpoint commit messages

Readable task commits make PR review and Git history easier to follow. SwarmOS uses the approved issue title and issue number as the commit subject, with a `Repair validation:` prefix for automated repairs and `Finalize:` for remaining changes checkpointed during submission. Subjects are kept to 72 bytes, preserving Unicode characters and the issue reference.

Choose the amount of context in `.swarm/config.yaml`:

```yaml
commit_message:
  detail: concise
```

- **`concise`** (default when omitted): a subject only, such as `Add backlog export (#15)`.
- **`detailed`**: the same subject plus the approved objective and a summary of files actually staged in that commit. The body includes up to 20 files, with an overflow count; long objectives and paths are shortened.

This setting applies to commits created by `swarm issue work`, `swarm issue stage --phase implement`, and `swarm issue submit`. Messages are derived from task context and Git changes without an additional model call. Commit the configuration before approving tasks: running tasks use the setting from their approved base, not edits in the candidate worktree. Existing commits are never renamed or amended.

---

## 3. Create, Lint, and Approve WorkUnit Issues

You can create structured WorkUnits from the GitHub Issue Form (`swarm:draft`), fenced YAML blocks, external `.swarm/workunits/<id>.yaml` references, roadmap files (`swarm repo plan --from <file> --apply`), or the CLI. Community members can submit bugs, ideas, and feedback through the **Community report** form (`swarm:community`), which remains intake until a maintainer scopes and approves a WorkUnit.

```bash
# Create a standalone non-executable Epic issue (requires maintainer/admin permission)
swarm issue epic create --backend github \
  --title "HTTP Client Resilience Epic" \
  --body-file plans/http-resilience.md

# Create a draft child WorkUnit linked as a native sub-issue of parent #10
swarm issue create --backend github \
  --parent 10 \
  --title "Add retry policy" \
  --objective "Retry transient HTTP failures" \
  --constraint "Preserve the public client API" \
  --acceptance "Retries 429 responses within the configured limit" \
  --workflow feature --gate human

# Validate the child issue schema and approve its revision for execution
swarm issue lint 42 --backend github
swarm issue approve 42 --backend github
```

Approving a child WorkUnit records its canonical WorkUnit digest, `.swarm/` policy digest, and trusted base SHA in Git state and applies the `swarm:ready` label. Epics (`type: Epic`) are non-executable planning containers: they carry no readiness label and cannot be approved or claimed directly.

To split an existing multi-item WorkUnit issue into linked `swarm:draft` child sub-issues, run:

```bash
swarm issue decompose 42 --sequential --backend github
```
---

## Validation policy and protected paths

`.swarm/validation/required-checks.yaml` defines the checks and protected paths that every task must satisfy:

```yaml
required_checks:
  - name: fmt
    command: unformatted=$(gofmt -l .) || exit 1; if [ -n "$unformatted" ]; then printf '%s\n' "$unformatted"; exit 1; fi
    timeout: 2m
  - name: build
    command: go build ./...
    timeout: 5m
  - name: test
    command: go test ./...
    timeout: 10m
  - name: lint
    command: go vet ./...
    timeout: 5m
protected_paths:
  - .github/workflows/**
  - .swarm/config.yaml
  - .swarm/validation/**
  - .swarm/contracts/**
  - .swarm/prompts/**
  - .swarm/workflows/**
```

During `swarm issue validate` and `swarm issue submit`, SwarmOS reads `required-checks.yaml` from the **approved base commit** rather than the candidate worktree, so a task cannot pass validation by weakening its own check commands or protected-path rules. Protected-path patterns use Go's `filepath.Match` syntax, where `*` stays within one directory. A `**` path component matches any number of directories: `**/.gitattributes` covers `.gitattributes` at the root and in every subdirectory, `a/**/b.yaml` covers `a/b.yaml` and `a/x/y/b.yaml`, and `dir/**` covers `dir` and everything below it. Negation (`!pattern`) and other gitignore features are not supported. Validation stops with an error naming any pattern it cannot parse, such as `[`. When a maintainer intentionally authors a task that updates `.swarm/` or `.github/workflows/`, set `allow_harness_changes: true` in that issue's WorkUnit before approving it.

For Go repositories, `swarm repo init` writes the `fmt`, `build`, `test`, and `lint` checks shown above. The `fmt` check fails when `gofmt -l` lists any file. Other stacks get `build`, `test`, and `lint` from their own toolchain.

`swarm repo doctor` compares the `run:` steps in `.github/workflows/*.yml` and `*.yaml` with these checks. When a workflow runs gofmt, `go vet`, staticcheck, golangci-lint, eslint, `prettier --check`, ruff, `black --check`, `cargo fmt --check`, or `cargo clippy` and no required check runs the same tool, doctor prints a `WARN` naming the workflow and command, with a suggested entry you can paste into `required_checks`. These warnings never fail doctor, and doctor never edits the policy: adding a check stays a maintainer change on the default branch. Doctor reads the commands as written. It does not follow `make` targets, scripts, reusable workflows, or `${{ }}` expressions, so a tool that only runs inside one of those is not reported.

### Build caches and who can write them

SwarmOS keeps compiled output between runs so agents and validation do not rebuild from scratch every time. It owns these caches itself, under your user cache directory (for example `~/.cache` on Linux), and never uses your own `GOCACHE` or ccache directory. Each repository gets its own key, a hash of its Git directory, so every worktree of one repository shares a cache and unrelated repositories never do.

| Cache | Path under the user cache directory | Written by | Read by |
| --- | --- | --- | --- |
| Agent | `swarm/agent-build/<repository key>/<ecosystem>` | Developer, tester and resolver runs | The same agent runs |
| Validator | `swarm/validate-build/<repository key>/<ecosystem>` | Warm-up of the approved base only | Candidate validation on Bubblewrap, through a discarded write layer |

The cached ecosystems are Go (`GOCACHE`), Rust (`CARGO_TARGET_DIR`), and C/C++ (`CCACHE_DIR`). .NET, Gradle and Maven are not cached yet, because their cache homes also hold credentials. Planner and reviewer runs are read-only and get a fresh scratch cache.

The two caches are never shared. Agent sandboxes never see the validator cache, and validation never reads an agent cache, so nothing an agent builds can reach the checks that judge its work.

Validation runs candidate code, which is untrusted, and build caches do not verify their entries when they read them. So candidate code never writes a persistent cache:

- **Warm-up.** On Bubblewrap with overlay support, the first validation at a new approved base commit first runs the required checks on a clean checkout of that base, with the validator cache writable. A `.warm-<base commit>` file in the repository's cache directory records that this ran. A failing base check is noted in the report and never changes the candidate's result.
- **Bubblewrap.** Candidate checks see the warmed cache read-only, with a temporary write layer that is thrown away when the check exits.
- **Codex native and host checks.** These include trusted-local execution and `swarm repo validate` on CI runners. They cannot layer a cache, so candidate checks get a fresh scratch cache. No warm-up runs there, because nothing would read the cache it writes.
- **OCI.** Validation and worker containers get no build cache.

If a cache directory cannot be prepared safely, for example because it is a symlink, writable by other users, or inside the worktree, that run uses a fresh scratch cache instead. The older validator cache at `swarm/go-build/<repository key>` moves to `swarm/validate-build/<repository key>/go` the first time validation runs.

Because Rust's `CARGO_TARGET_DIR` points at the agent cache, agent runs do not leave a `target/` directory in the workspace, and concurrent runs for one repository wait on Cargo's build-directory lock. You can delete any of these directories at any time to reclaim space.

**Adding an ecosystem:** add an entry to `buildCacheEcosystems` in `internal/sandbox/build_cache.go`, using an environment variable that names only a build-output directory, never a credential or dependency home, and extend `TestBuildCacheTableSafety`. Ecosystems whose cache home holds credentials stay deferred.

**Changing who can write:** every validator mount is chosen by `ValidatorCacheMounts`, and whether warm-up runs is chosen by `ValidatorCacheReusable`, both in `internal/sandbox/build_cache.go`. Any change that lets candidate code write a shared cache needs a maintainer decision and an update to `SECURITY.md`.

### Dependency caches

Sandboxes have no network, so downloaded dependencies must already be on disk. For Go and Rust, SwarmOS does not mount your own `GOMODCACHE` or Cargo `registry` and `git` caches, which hold everything you have ever fetched, including private dependencies. Each repository gets its own cache instead, filled on the host before any agent or validation run starts, and mounted read-only in every sandbox.

| Cache | Path under the user cache directory | Filled for | Read by |
| --- | --- | --- | --- |
| Agent | `swarm/agent-deps/<repository key>/<go, rust or python>` | Planner, reviewer, developer, tester and resolver runs | Those runs, read-only |
| Validator | `swarm/validate-deps/<repository key>/<go, rust or python>` | Validation, including warm-up | Validation checks, read-only |

The prefetch runs `go mod download -json` on a temporary copy of the approved base's `go.mod` and `go.sum`, never the candidate worktree. It reads modules from your own module cache first and then from your `GOPROXY`. It runs with a fresh environment: no GitHub token, `SWARM_GITHUB_TOKEN_COMMAND`, SSH agent, Git credential helper or `GOFLAGS`, and with `GOTOOLCHAIN=local`, so it only downloads, verifies and extracts. When `go.mod` asks for a newer Go than the one installed, the prefetch also downloads that Go release so sandboxed `go` commands can switch to it offline. A `.prefetched-go-<digest>` file records each completed prefetch, so it runs again only when `go.mod` or `go.sum` changes.

For Rust, a root `Cargo.lock` in the approved base enables the cache, and sandboxes see it as a read-only `CARGO_HOME`. The prefetch runs `cargo fetch --locked` on a temporary copy, beside the cache, of the base's `Cargo.lock` and every `Cargo.toml` with an empty `src/lib.rs` stub; no `build.rs`, `.cargo/config.toml` or `rust-toolchain` file is copied, so no build script, rustc wrapper or repository toolchain runs. It runs with a fresh environment: no GitHub token, SSH agent, registry token, `RUSTFLAGS` or rustc wrapper, and rustup does not install toolchains. Your own `~/.cargo/config.toml`, including registry mirrors, does not apply. The prefetch fails if the cache then holds anything besides Cargo's `registry` and `git` downloads and lock files, so no Cargo `config.toml` or `credentials.toml` reaches a sandbox. A `.prefetched-rust-<digest>` file records each completed prefetch, so it runs again only when `Cargo.lock` or a `Cargo.toml` changes.

Unlike build caches, dependency caches never fall back. If the cache directory cannot be prepared safely, or a module cannot be downloaded, `swarm issue work` and `swarm pr work` stop before claiming the task, and validation stops before any check runs. The error names the module or crate, for example `prefetch go modules for base 1a2b3c4d5e6f: example.com/missing@v1.0.0: ...` or `prefetch rust modules for base 1a2b3c4d5e6f: dependency missing-crate: ...`. Only the root `go.mod` is detected; nested modules and `go.work` get no prefetch.

#### Python

A repository whose approved base has a root `uv.lock` or `requirements*.txt` gets its own Python wheel cache, and sandboxes no longer see your pip, uv or Poetry caches. uv-managed interpreters stay mounted. The prefetch never builds a package and never runs package code:

- For `uv.lock`, it runs `uv export --frozen --offline --no-build --no-config` on a temporary copy of the base's `uv.lock` and `pyproject.toml`, which only reads the lockfile.
- For the export and each `requirements*.txt`, it runs `python3 -I -m pip download --only-binary=:all:` into the cache. pip downloads wheels only, so an sdist's `setup.py` or build backend never runs. The export is downloaded with `--no-deps --require-hashes`.

Before pip runs, every requirements file must contain only registry requirements (a name, extras, version specifiers and markers), `--hash` options, and `-r` or `-c` references to another root `requirements*.txt`. Editable installs, paths, URLs, VCS and `name @ url` references, `${VAR}` expansion and every other option, such as `--no-binary` or `--index-url`, are rejected with the file and line. The prefetch environment has no GitHub token, SSH agent, Git credential helper, pip or uv configuration file, or `PYTHONPATH`; it copies only `PATH`, `PIP_INDEX_URL`, `PIP_EXTRA_INDEX_URL`, `PIP_TRUSTED_HOST`, `PIP_CERT` and `SSL_CERT_FILE`.

A dependency that publishes only an sdist is reported, not built, and the run stops before it claims work, for example `prefetch python modules for base 1a2b3c4d5e6f: requirements.txt: missing==2.0: no compatible wheel (sdist-only dependencies are not built)`. Wheels are chosen for the host's `python3`.

Sandboxes find the wheels through `PIP_FIND_LINKS`, so `pip install --no-index -r requirements.txt` works offline. With uv, run `uv pip install --offline --find-links "$PIP_FIND_LINKS" -r requirements.txt`. A Python repository with neither file, for example a Poetry project, keeps read-only access to your host pip, uv and Poetry caches.

Other ecosystems have no fetch-only download command yet, so they keep read-only access to your host caches, as before:

| Ecosystem | Host caches still mounted read-only | Why |
| --- | --- | --- |
| Node | npm, pnpm, Yarn, Bun, Playwright, Cypress | Adapters are planned |
| .NET | NuGet packages | Restore runs MSBuild |
| Maven | `~/.m2/repository` | Plugins run during resolve |
| Gradle | `~/.gradle/caches`, `~/.gradle/wrapper` | Build scripts run during resolve |
| C/C++ | vcpkg binary cache | vcpkg ports run build scripts |

`swarm repo doctor` shows a `dependency-caches` check that lists each ecosystem's mode and names those sharing host caches. `swarm node status` shows the same, and `swarm node status --json` reports it as `dependency_caches`.

To mount your host-wide Go module cache, Cargo caches and Python caches read-only again on one machine, set `share_host_dependency_caches: true` in the node profile (`~/.config/swarm/node.yaml`, or the file named by `SWARM_NODE_CONFIG`). This is off by default, it is a node setting only, and `.swarm/config.yaml` rejects it. Doctor reports the opt-in as a `WARN`. Validation in CI always uses per-repository caches.

**Adding an ecosystem:** add an entry to `dependencyEcosystems` in `internal/sandbox/dependency_cache.go`. An adapter needs a `Prefetch` that runs only a fetch-only command; anything that can run package code stays `NoPrefetch`. Every `addCache` call in `internal/sandbox/toolchain.go` names its ecosystem, and `TestEveryMountedEcosystemIsClassified` fails until the ecosystem is in the table.

Released builds can reuse a coordinator-recorded passing validation at `swarm issue submit` or `swarm pr submit` when the checkpoint tree, approved base, policy digest, worker, claim, and swarm version still match. Submit shows the original run's age and short tree SHA in its output and PR evidence. Explicit validation, CI, development builds, and fork submissions continue to run checks.

---

## 4. Plan Review Gates (`none | agent | human`)

- **`none`**: Runs the planner to generate `PLAN.md` and `TEST_PLAN.md`, then proceeds directly to implementation and validation.
- **`agent`**: Runs your configured reviewer agent inside an isolated read-only snapshot. Implementation starts only when `.swarm/active/REVIEW.md` begins with `VERDICT: APPROVED` (bound to the current WorkUnit, plan, and test-plan digests).
- **`human`**: Pauses after publishing the plan so a maintainer can inspect the design and run `swarm issue review-plan <issue> --approve` or `swarm issue review-plan <issue> --reject`.

A CLI `--gate` flag can always make review stricter (for example, upgrading `none` to `agent` or `human`), while preserving the minimum gate required by the issue and repository policy.

`swarm issue approve <issue>` authorizes the issue's WorkUnit contract for execution; `swarm issue review-plan <issue> --approve` separately approves the implementation plan before code changes. `swarm issue submit <issue>` links a Draft PR proposing the implementation. Continue development with `swarm pr work <PR>` or update ancestry with `swarm pr rebase <PR>`, then validate and publish with `swarm pr submit <PR>`. A changed target baseline requires approval of its policy; PR revisions do not bypass human plan gates. Review code and acceptance criteria in GitHub before merging.

---

## Related Guides

- **[Day in the Life of a Repo Admin (`docs/ADMIN_GUIDE.md`)](ADMIN_GUIDE.md)** — Daily maintainer playbook for `swarm repo plan`, triage labels, and Draft PR provenance review.
- **[GitHub Security, Branching & Agent Authorization (`docs/GITHUB.md`)](GITHUB.md)** — Configure repository rulesets, `SWARM_GITHUB_TOKEN`, and role-separated collaborator accounts.
- **[Developer & Worker Guide (`docs/developer.md`)](developer.md)** — Everyday CLI workflows, interactive `swarm issue stage`, agent harness table, reviewer isolation, checkpoints, and recovery.
- **[Working with Grok Bot (`docs/grokbot.md`)](grokbot.md)** — Connect Grok Bot to GitHub and coordinate SwarmOS tasks.
- **[Working with OpenAI Dot & Codex (`docs/openai-dot.md`)](openai-dot.md)** — Configure Codex Cloud environments and remote repositories for OpenAI Dots.
- **[Workflows & Review Gates (`docs/WORKFLOWS.md`)](WORKFLOWS.md)** — Role prompts, artifacts, and gate transitions.
- **[Architecture (`docs/ARCHITECTURE.md`)](ARCHITECTURE.md)** — Git-CAS coordination, fencing, and worktree lifecycle.
