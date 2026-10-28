---
description: Execute the SwarmOS Plan phase (analyze, design PLAN.md, and produce TEST_PLAN.md)
---

Read `.swarm/active/WORKUNIT.md` and the approved role instructions in `.swarm/active/SYSTEM_PROMPT.md`.
Inspect the repository without modifying production source files.
Produce:
1. `.swarm/active/PLAN.md` with **What we know** (with evidence), **What we're assuming**, and **Open questions**, followed by exact files, symbols, and step-by-step implementation details. If WorkUnit dimensions are absent, state suggested size, uncertainty, and risk as advisory. Only the compiled approved execution profile supplies variant requirements.
2. `.swarm/active/TEST_PLAN.md` mapping every Acceptance Criterion in `.swarm/active/WORKUNIT.md` to concrete test cases and verification commands.
