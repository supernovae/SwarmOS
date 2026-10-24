---
description: Execute the SwarmOS Plan phase (analyze, design PLAN.md, and produce TEST_PLAN.md)
---

Read `.swarm/active/WORKUNIT.md` and the approved role instructions in `.swarm/active/SYSTEM_PROMPT.md`.
Inspect the repository without modifying production source files.
Produce:
1. `.swarm/active/PLAN.md` that opens with the work's **Shape** (Routine, Involved, Exploratory, or Urgent), **What we know** (with evidence), **What we're assuming**, and **Open questions**, followed by exact files, symbols, and step-by-step implementation details. Scale the detail to the shape: a Routine fix stays short; Exploratory work starts with a probe and lists follow-ups.
2. `.swarm/active/TEST_PLAN.md` mapping every Acceptance Criterion in `.swarm/active/WORKUNIT.md` to concrete test cases and verification commands.
