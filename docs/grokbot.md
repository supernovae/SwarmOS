# Working with Grok Bot and SwarmOS

Grok Bot can be the teammate you talk to about a repository: ask it to explore the code, turn an idea into a task, follow a change through to a PR, or give you another review. SwarmOS gives that work a shared backlog and a repeatable delivery path, so Grok does not have to coordinate everything through chat history.

There are two useful ways to put them together:

- **A repository teammate:** connect Grok Bot to GitHub and talk to it about issues, plans, and pull requests.
- **A worker coordinator:** give its computer a checkout and `swarm`, then ask it to run approved work through the coding tools configured for that repository.

A GitHub connection is enough for repository conversation and supported review actions. Running `swarm` also needs a shell, the CLI, repository access, and the configured coding tools on the computer doing the work.

## Connect Grok Bot to your repository

1. Create a Bot in the [Grok Bot app](https://x.ai/bot). Give it a focused job, such as “help maintain this repository” or “review proposed changes.” The [getting-started guide](https://docs.x.ai/grok-bot/get-started) walks through account setup.
2. Open **Connect apps**, add the [GitHub connector](https://x.ai/bot/plugin/48677658/github), and complete authentication. In a conversation, type `@` to attach the connector to the task. See [Grok's app connection guide](https://docs.x.ai/grok-bot/computer-and-apps#connect-an-app).
3. Grant access to the repositories you want it to help with. If your setup uses a separate bot-owned GitHub account, invite that verified account as a collaborator with the permissions its job needs. If it uses an app installation or your connection, configure repository access there instead; a collaborator invite is not always necessary.
4. Start with a read-only request: ask it to open the repository, explain the project, and link an issue and a PR it can read. Then try a review comment on a test PR before granting broader writing access.

Bots on your Grok account share a computer, files, and signed-in sessions. Choose credentials with that shared access in mind; creating another Bot does not create a separate credential boundary.

## Talk to the repository

You can give Grok a normal request rather than a long system prompt. For example:

> Get familiar with OWNER/REPO. Read the README and the committed `.swarm/` project, architecture, and conventions files. Explain what the project does, identify the next useful piece of work, and draft an issue with an objective, constraints, and acceptance criteria. Leave it as a draft for me to approve.

Or ask it to investigate an existing task:

> Look at issue #42 in OWNER/REPO. Check the acceptance criteria against the code and tests. Tell me what is missing and propose a plan. Do not start implementation or approve the task on my behalf.

The repository stays the durable record. Keep product discussion wherever it is useful, but put the agreed scope in a GitHub WorkUnit and approve it through `swarm issue approve` before a worker starts.

## Use Grok as a GitHub reviewer

Invite or connect the Bot using the access method above, then give it the exact PR and what to look for:

> Review OWNER/REPO PR #123 against issue #42. Read the plan and test evidence linked from the PR. Look for correctness gaps, missing acceptance coverage, and security concerns. Post actionable findings with file and line references. Do not change code, approve the SwarmOS plan, or merge the PR.

You can make that a recurring job once the first review works. Grok's marketplace also offers a third-party [Grok Bug Bot template](https://x.ai/bot/mXAP6VMDNxs4uLiv2B2HW) for selected repositories; its published behavior is comment-only PR review, not approval or merging. Review the template and requested permissions before adding it.

**A GitHub review and a SwarmOS plan gate are different steps.** Bot comments are useful review input, but SwarmOS does not turn a mention or `/swarm approve-plan` comment into approval. For a human-gated task, a maintainer reads the findings and records their own decision:

```bash
swarm review 42 --approve --notes "Reviewed the plan and Grok's findings."
swarm work 42
```

Use `--reject --notes "..."` when changes are needed. An automated SwarmOS agent gate instead runs a configured reviewer inside the review sandbox and records its plan-bound `REVIEW.md` verdict; see the [integration runbook](developer.md#reviewer-isolation-and-credentials).

## Let Grok coordinate approved work

On the computer Grok will use, install the pinned SwarmOS version from the [README](../README.md#quickstart), Git, `gh`, and the coding tools allowed by the repository. Authenticate the coordinator for the intended GitHub repository. Initialize and commit `.swarm/` if the project has not been set up yet.

From each checkout:

```bash
swarm doctor --backend github
swarm repo add --backend github
```

Then give the Bot a short operating brief:

```text
Work on OWNER/REPO using SwarmOS from its registered checkout.
Read README.md and the committed .swarm/ project guidance first.
Use `swarm work 42 --dry-run` to show me the configured agents and policy.
When I authorize execution, use `swarm work 42`; let SwarmOS run the roles.
If it pauses for human review, bring me the plan and wait for my decision.
When work and validation pass, use `swarm submit 42` and send me the Draft PR.
Do not approve tasks, change policy, or merge unless I separately authorize it.
If credentials, tools, or the sandbox are missing, report the blocked step.
```

Inside a role, the coding harness reads `.swarm/active/WORKUNIT.md`, `SYSTEM_PROMPT.md`, and the current plan/test-plan artifacts. The conversational Bot runs the coordinator commands outside those role processes; it should not start another `swarm work` recursively inside a running agent role.

For a backlog rather than a named issue, use `swarm work --next --all`. For a bounded pool on that prepared host:

```bash
swarm node init --id grok-coordinator
swarm node run --all --concurrency 2 --max-jobs 4 --auto-submit
```

This drains up to four task/phase attempts using the repository's configured agents. Add `--watch` for continued polling. The Bot's connection to GitHub does not install those tools or automatically register repositories on its computer.

## Which part is Grok, and which part is SwarmOS?

Grok Bot is the conversational teammate with its own tools and computer. SwarmOS currently launches the `claude`, `codex`, `opencode`, and `omp` harnesses for its internal roles; it does not launch the Grok Bot app through a `grok -p` command. Grok can still drive those harnesses through `swarm` or review their output through GitHub.

If you want a Grok **model** inside an automated role, use a supported harness with an xAI provider, such as [OpenCode's xAI integration](https://opencode.ai/docs/providers/#xai). Configure that installed harness's provider, exact model, and model-auth environment first, then select it in `.swarm/config.yaml`. That is a model-backed harness configuration, separate from connecting the hosted Grok Bot. The shared [agent runbook](developer.md#agent-integration-runbook) covers model routing and reviewer network access.

Related: [project setup](project.md) · [worker operation and recovery](developer.md) · [review gates](WORKFLOWS.md) · [security](../SECURITY.md).
