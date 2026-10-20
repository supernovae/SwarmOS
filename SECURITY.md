# Security Policy

## Supported Versions

| Version | Supported |
| :--- | :--- |
| `0.x` (latest tagged release & `main`) | :white_check_mark: |

## Reporting a Vulnerability

Please report security vulnerabilities privately via **[GitHub Security Advisories](https://github.com/supernovae/SwarmOS/security/advisories/new)** rather than opening a public issue.

We will acknowledge receipt within **48 hours** and provide a remediation timeline or patch release as quickly as possible.

---

## Built-In SwarmOS Security Controls

SwarmOS executes AI coding agents against GitHub repositories and therefore enforces multiple defense-in-depth controls by default:

1. **Issue Trust Gate (`security.require_ready_label` & `trusted_associations`)**:
   - Untrusted external users cannot trigger agent execution. An issue must either carry the maintainer-controlled `swarm:ready` label or be authored by a user with `OWNER`, `MEMBER`, or `COLLABORATOR` association (`ErrUntrustedIssue`).
2. **Prompt Injection Sanitization (`StripHTMLComments`)**:
   - All hidden HTML comments (`<!-- ... -->`) are stripped from issue bodies before parsing `WorkUnit` contracts so invisible instructions cannot be smuggled into `.swarm/active/SYSTEM_PROMPT.md`.
3. **Protected-Path Tamper Detection (`ErrProtectedPathModified`)**:
   - `swarm validate` blocks any modification to `.github/workflows/**`, `.swarm/validation/**`, or `.swarm/contracts/**` unless `allow_harness_changes` is explicitly granted.
4. **CI/CD & Supply-Chain Hardening**:
   - **CodeQL SAST** & **`govulncheck`** run on every push, pull request, and weekly schedule.
   - **Dependency Review** gates pull requests against high/critical CVEs.
   - **SLSA Build Provenance Attestations** (`actions/attest-build-provenance`) are published for all tagged release archives (`linux`, `darwin`, `windows` on `amd64` and `arm64`), verifiable via:
     ```bash
     gh attestation verify swarm_<version>_<os>_<arch>.tar.gz --repo supernovae/SwarmOS
     ```
