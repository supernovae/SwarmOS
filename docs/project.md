# Project & Maintainer Guide

This guide explains how repository maintainers initialize SwarmOS, configure governance and validation rules in `.swarm/`, author contract-driven `WorkUnit` issues, and partition larger epics into dependency-ordered subtasks.

---

## 1. Initializing SwarmOS in a Repository

Run `swarm init` inside any Git repository:

```bash
# Scaffold .swarm/ and create swarm:* issue labels on GitHub
swarm init --sync-labels

# Verify repository contracts, git, GitHub auth, and installed agent binaries
swarm doctor
```

`swarm init` automatically inspects your repository (`go.mod`, `Cargo.toml`, `pyproject.toml`, `package.json`) and generates:

| Path | Purpose |
| :--- | :--- |
| `.swarm/config.yaml` | Default agent harness, plan review gate, lease TTL/renewal limits, role-to-agent mappings, and security trust policy. |
| `.swarm/project.md` | Detected stack profile, build command, and test command injected into agent system prompts. |
| `.swarm/architecture.md` | High-level architectural boundaries and invariants for the project. |
| `.swarm/conventions.md` | Coding and testing conventions enforced across all agents. |
| `.swarm/workflows/{feature,bugfix,refactor}.yaml` | Step-by-step execution workflows mapping steps to agent roles (`planner`, `reviewer`, `developer`, `tester`). |
| `.swarm/prompts/{planner,reviewer,developer,tester}.md` | Role prompt contracts governing what each role reads and produces. |
| `.swarm/contracts/{workunit,artifact}.schema.json` | JSON Schemas (Draft 2020-12) validating work units and published phase artifacts. |
| `.swarm/validation/required-checks.yaml` | Commands that must pass before a PR can be submitted, plus `protected_paths` globs. |
| `.github/ISSUE_TEMPLATE/swarm-workunit.yml` | Native GitHub Issue Form so contributors can create valid WorkUnits directly from the GitHub UI. |
| `.github/workflows/swarm-validate.yml` | GitHub Actions workflow running tests and `swarm validate --ci` on pushes and PRs. |
| `.claude/commands/swarm-*.md` & `.opencode/command/swarm-*.md` | Universal slash commands (`/swarm-plan`, `/swarm-review`, `/swarm-implement`, `/swarm-validate`). |

Commit the generated `.swarm/`, `.github/`, `.claude/`, and `.opencode/` files to your default branch so every contributor and agent shares the exact same project harness.

---

## 2. Configuring `.swarm/config.yaml`

```yaml
version: 1
default_agent: claude
default_gate: agent

# Optional: Route specific workflow roles to specialized agent harnesses
roles:
  planner: claude
  reviewer: omp
  developer: codex
  tester: opencode

lease:
  duration: 4h
  renewable: true
  max_renewals: 3

security:
  require_ready_label: true
  ready_label: "swarm:ready"
  trusted_associations: ["OWNER", "MEMBER", "COLLABORATOR"]

agents:
  claude:
    binary: claude
  codex:
    binary: codex
  opencode:
    binary: opencode
  omp:
    binary: omp
```

### Security & Trust Policy
On public repositories, untrusted users cannot add labels to GitHub Issues. With `require_ready_label: true`, SwarmOS refuses to claim or execute any issue unless:
1. A maintainer has applied the `swarm:ready` label, **or**
2. The issue author's GitHub association is in `trusted_associations` (`OWNER`, `MEMBER`, `COLLABORATOR`).

Additionally, SwarmOS strips all `<!-- ... -->` HTML comments from issue bodies before parsing so hidden prompt-injection payloads cannot reach agent system prompts.

---

## 3. Configuring Validation & Protected Paths

`.swarm/validation/required-checks.yaml` defines the automated checks that `swarm validate` and `swarm submit` enforce inside every worktree:

```yaml
required_checks:
  - name: build
    command: go build ./...
    timeout: 5m
  - name: test
    command: go test -race ./...
    timeout: 10m
  - name: lint
    command: go vet ./...
    timeout: 5m

protected_paths:
  - .github/workflows/**
  - .swarm/validation/**
  - .swarm/contracts/**
```

If an agent modifies any file matching `protected_paths` (for example, attempting to weaken CI or validation checks) and the WorkUnit does not explicitly grant `allow_harness_changes: true`, validation fails immediately with `ErrProtectedPathModified`.

---

## 4. Creating & Linting WorkUnit Issues

Every SwarmOS task is a GitHub Issue (or offline local issue) containing a structured `WorkUnit` contract.

### Option A: Via the CLI (`swarm issue create`)
```bash
swarm issue create \
  --title "Add rate-limit retry middleware" \
  --objective "Implement exponential backoff retry middleware for HTTP 429 and 503 responses" \
  --constraint "Use standard library net/http only" \
  --constraint "Do not alter existing Client interface" \
  --acceptance "Retries HTTP 429 and 503 up to max_retries" \
  --acceptance "Unit tests verify Retry-After header parsing" \
  --workflow feature \
  --gate agent \
  --capability coding,testing
```

### Option B: Via the GitHub Issue Form UI
Open a new issue on GitHub and choose **SwarmOS Work Unit** (`.github/ISSUE_TEMPLATE/swarm-workunit.yml`). Fill in the **Objective**, **Constraints**, **Acceptance Criteria**, **Workflow** dropdown, and **Plan Gate** dropdown.

### Option C: Via Fenced YAML in Any Issue Body
```yaml swarm-workunit
id: ISSUE-42
objective: Add OAuth2 device flow support
constraints:
  - Do not break existing token storage
acceptance:
  - Device flow integration test passes
workflow: feature
gate: agent
depends_on: [40, 41]
required_capabilities: [coding, testing]
```

### Linting Existing Issues
To verify that an existing issue conforms to `.swarm/contracts/workunit.schema.json`:
```bash
swarm issue lint 42
```

---

## 5. Partitioning Epics & Dependency Ordering (`swarm decompose`)

When an issue has multiple acceptance criteria or subtasks that should be worked on incrementally (or in parallel by multiple contributors/agents), partition it with `swarm decompose`:

```bash
# Create linked child issues chained sequentially via depends_on (#2 -> #3 -> #4)
swarm decompose 10 --sequential

# Or create independent parallel child issues (--sequential=false)
swarm decompose 10 --sequential=false
```

- Each child issue records `### Parent Issue: #10`.
- When `--sequential` is used (or `--depends-on` is specified), `swarm pull` and `swarm work` automatically skip downstream child issues until their prerequisite issues are closed, completed, or have an open Draft PR (`swarm:pr-open`).
- A summary table of all created child issues and their dependency graph is posted as a comment on the parent issue.

---

## 6. Reviewing Plans & Managing Claims

Maintainers can review proposed agent plans or manage leases either from the CLI or directly in GitHub Issue comments:

| Action | CLI Command | GitHub Issue Comment |
| :--- | :--- | :--- |
| Approve a plan (`gate: human`) | `swarm review <issue> --approve --notes "LGTM"` | `/swarm approve-plan` |
| Request plan changes | `swarm review <issue> --reject --notes "Use stdlib"` | `/swarm reject-plan` |
| Run an automated reviewer agent | `swarm review <issue> --agent omp` | — |
| Release a stale/active lease | `swarm release <issue> --reason "Reassigning"` | `/swarm release` |
| Inspect queue & active leases | `swarm status [<issue>]` | Living `<!-- swarm:state:v1 -->` comment |
