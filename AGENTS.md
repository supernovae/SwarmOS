# AGENTS.md — SwarmOS Agent & Contributor Guide

Welcome to **SwarmOS (`github.com/supernovae/swarmos`)**. Every coding agent, reviewer bot, and human contributor working in this repository should read this file first.

---

## 1. The SwarmOS Vision & Voice

SwarmOS turns GitHub and Git into a shared, zero-infrastructure coordination layer where people and coding agents can pool compute to ship real software together:

- **For Open Source Communities:** Maintainers define and approve structured `WorkUnit` issues. Contributors anywhere in the world can point their own coding agents (`claude`, `codex`, `opencode`, `omp`, Grok Bot, or OpenAI Dot) at the repository with `swarm issue work --next` or `swarm node run`, donate agent compute to tackle real backlog items in isolated Git worktrees, and return validated, artifact-backed Draft PRs.
- **For Internal Engineering Organizations:** Engineering teams can run the exact same workflow across private GitHub repositories—using GitHub Issues, Git compare-and-swap state refs, and existing CI runners as a zero-infrastructure control plane to orchestrate internal coding agents without standing up a separate database or proprietary orchestration server.

When writing code, prompts, or documentation in this repository, preserve that spirit: **practical, welcoming, GitHub-native, and built for real builders.**

---

## 2. Documentation Guardrails (Preventing "Prompt Bleed-Through")

Our documentation (`README.md`, `docs/*.md`, `SECURITY.md`) is the front door to the project. When editing any Markdown file, follow these rules strictly:

1. **Write for humans, never for an audit rubric.**
   - Explain *why* a feature helps a developer, maintainer, or engineering team before diving into mechanics.
   - **Never leak system-prompt phrasing, grading rubrics, or defensive legalese into user-facing docs.** Avoid robotic disclaimer chains such as *"do not infer that X is Y merely because Z exists"*, *"this proves startup, not authenticated model execution; no paid model calls were made"*, or *"no vendor compatibility guarantee is asserted here."*
2. **State boundaries constructively and clearly.**
   - Always be 100% honest about what `swarm` does today, how trust boundaries work (e.g., worktrees isolate Git checkouts while reviewer sandboxes isolate `reviewer` runs), and what requires maintainer approval.
   - Phrase operational requirements as clear, actionable setup steps (`> [!TIP]`, `> [!NOTE]`, numbered checklists) rather than skeptical warnings that talk down to the reader or external tools.
3. **Preserve visual flavor and GitHub-native design.**
   - Keep the centered header, badges, Mermaid architecture/workflow diagrams, comparison tables, and CLI quick-reference tables intact in `README.md`.
   - Keep the product visuals (`docs/assets/grokbot-*.png`, `docs/assets/openai-dot-*.png`), natural conversational prompts ("talk to the repo"), and step-by-step cloud/remote setup flows in `docs/grokbot.md` and `docs/openai-dot.md`.
4. **Every command and flag must be real.**
   - Never document invented flags, subcommands, environment variables, or config keys. Verify every CLI example against `cmd/swarm` and `internal/cli/` (`swarm --help`, `swarm <subcommand> --help`).
   - Quote or avoid special characters (such as leading `@`) in Mermaid edge labels so GitHub's Mermaid renderer never fails with `Unable to render rich display`.

---

## 3. Codebase Map & Architectural Invariants

- **`cmd/swarm/` & `internal/cli/`**: Cobra CLI entrypoint and resource groups (`repo init`, `repo doctor`, `repo plan`, `repo validate`, `issue create/approve/claim/work/stage/review-plan/validate/submit/status/renew/release`, `pr status/work/rebase/validate/submit`, `node init/status/run`), plus root `version` and `fork-coordinator`.
- **`internal/contract/`**: Canonical `WorkUnit`, `Lease`, `Artifact`, `WorkflowSpec`, `ValidationSpec`, and `.swarm/config.yaml` types, JSON Schema validation, and HTML comment stripping.
- **`internal/coordination/` & `internal/lease/`**: Git compare-and-swap state refs (`refs/heads/swarm-state/issue-<N>`), file-locked local CAS store (`--backend local`), fenced claims (`claim_id`), heartbeats, and issue audit comments.
- **`internal/workspace/`**: Isolated Git worktree provisioning (`.swarm/worktrees/issue-<N>`), checkpoint commits, and active bundle compilation (`.swarm/active/WORKUNIT.md`, `SYSTEM_PROMPT.md`). Note that SwarmOS injects an ephemeral `` pointer block into `AGENTS.md` and `CLAUDE.md` inside task worktrees and strips it automatically before checkpoints and submissions—never delete content outside those markers.
- **`internal/agent/` & `internal/sandbox/`**: Adapters for `claude`, `codex`, `opencode`, `omp`, and local/test `mock`, plus isolated read-only snapshot sandboxing (`sandbox-exec`, `bwrap`, or OCI `docker`/`podman`) for the `reviewer` role.
- **`internal/workflow/` & `internal/validation/`**: Plan-first execution engine (`plan` $\rightarrow$ `gate` $\rightarrow$ `implement` $\rightarrow$ `validate`), cryptographic binding of review verdicts to `WorkUnit`/`PLAN.md`/`TEST_PLAN.md` digests, and baseline-pinned validation of `required-checks.yaml` and `protected_paths`.
- **`internal/pr/`**: Continued development of PRs authoritatively linked to approved WorkUnits. PR commands use PR numbers, retain the linked issue's fenced claim and approval gates, prepare changes in separate PR worktrees, and publish only through explicit `pr submit` with an expected remote head.

---

## 4. Verification Checklist Before Opening or Updating a PR

```bash
# Format, vet, static analysis, and race-enabled test suite
gofmt -s -w .
go vet ./...
go test -race ./...
```

When touching documentation:
- Confirm all relative file links, image paths (`docs/assets/*`), and heading anchors resolve.
- Confirm every `bash` snippet parses cleanly (`bash -n`) and uses real `swarm` flags.

---

## Code Review Rules

### Documentation voice and accuracy

- Flag any documentation change that introduces prompt bleed-through, internal rubric phrasing, or defensive legalese into `README.md` or `docs/*.md`.
  Safe path: rewrite in clear, human-friendly developer prose while preserving accurate CLI flags and security boundaries.
- Flag any documented `swarm` command, flag, or `.swarm/config.yaml` field that does not exist in `internal/cli/` or `internal/contract/`.
  Safe path: match the exact flags registered on the Cobra commands and structs.

### Coordination and validation safety

- Do not read authoritative validation policy (`required-checks.yaml` or `protected_paths`) from an untrusted candidate worktree when a trusted `base_sha` is available.
  Safe path: load validation policy and role prompts from the approved base commit (`baseSHA`).
