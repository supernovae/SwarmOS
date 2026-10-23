# SwarmOS Roadmap — Next Capability Wave

This roadmap defines the next wave of SwarmOS capabilities for repository maintainers, contributors, and coding agent operators. Decompose or preview these work units with:

```bash
swarm repo plan --from ROADMAP.md
```

## Add provenance attestation inspection to swarm issue status

Extend `swarm issue status` in `internal/cli/status.go` so maintainers and operators can inspect per-role provenance attestations directly from the terminal without opening the GitHub issue thread.

- [ ] Display recorded per-role provenance (`planner`, `reviewer`, `developer`, `tester`) including harness, version, model, sandbox boundary, and artifact SHA-256 digests when inspecting a specific issue (`swarm issue status <issue>`) in `internal/cli/status.go`
- [ ] Include structured provenance records in `swarm issue status --json` output
- [ ] Add unit test coverage in `internal/cli/cli_test.go` verifying human-readable and JSON status output with provenance attestations

## Add --export flag to swarm repo plan for versioned backlog snapshots

Allow maintainers to export the structured `ProjectPlanReport` produced by `swarm repo plan` in `internal/cli/plan.go` and `internal/workflow/project_plan.go` to a YAML or JSON snapshot file for versioned release planning and CI artifacts.

- [ ] Add `--export <path>` flag to `swarm repo plan` in `internal/cli/plan.go` supporting `.yaml`/`.yml` and `.json` output formats
- [ ] Write deterministic YAML/JSON serialization of `ProjectPlanReport` in `internal/workflow/project_plan.go`
- [ ] Add unit tests covering `swarm repo plan --scan --export` and `swarm repo plan --from ROADMAP.md --export`

## Add doctor check for reviewer sandbox runtime readiness

Extend `swarm repo doctor` in `internal/harness/scaffold.go` and `internal/sandbox/sandbox.go` to report the active read-only reviewer sandbox backend (`sandbox-exec` on macOS, `bwrap` on Linux, or `docker`/`podman` OCI container) so operators can verify reviewer isolation before claiming tasks.

- [ ] Add a `sandbox` category check to `RunDoctor` in `internal/harness/scaffold.go` using runtime detection from `internal/sandbox/sandbox.go`
- [ ] Report `OK` with the resolved sandbox backend binary path when an isolation runtime is available, or `WARN` with actionable installation guidance when missing
- [ ] Add unit tests verifying the doctor sandbox readiness check
