# Working with OpenAI Dot, Codex, and SwarmOS

Your dot can be the teammate who keeps a project moving: talk through an idea, ask it to inspect a repository, delegate coding to Codex, and come back with a PR or a decision for you. SwarmOS adds a shared work queue, explicit approvals, and checkpoints so that work can move between your dot, coding agents, and people.

OpenAI documents that [dots can create tasks in Codex cloud environments](https://help.openai.com/en/articles/20001530-getting-started-with-your-dot). Create the environment first, connect the repositories you want it to use, and then tell your dot which environment and project to work in. You do not need a separate `dot` executable for this workflow.

## Set up your dot and repository access

1. Create your dot at [chatgpt.com/dots](https://chatgpt.com/dots), using the intended account and workspace. Follow OpenAI's [getting-started guide](https://help.openai.com/en/articles/20001530-getting-started-with-your-dot) for availability and onboarding.
2. Connect GitHub through your supported ChatGPT/Codex connection. In GitHub's app or connection settings, select the repositories you want to work on and complete any organization or SSO approval.
3. Review the dot's connected plugins and action rules. [Plugin permissions are shared across dots, ChatGPT, and Codex](https://help.openai.com/en/articles/20001529-dots-privacy-security-and-safety-faqs); a new conversation is not a separate access boundary.
4. Ask your dot to read one selected repository and summarize its README and `.swarm/project.md`. Confirm it returns the right repository and branch before asking it to change anything.

Repository access and a ready-to-run coding environment are separate. The next step makes the repositories available where Codex will actually run.

## Add the repositories to Codex Cloud

Follow the current [cloud environment guide](https://learn.chatgpt.com/docs/environments/cloud-environments):

1. On the web or desktop app, choose **Work in → Cloud → Select environment → Create environment**. You can also start from **Settings → Codex Cloud → Environments**.
2. Select the GitHub repositories to check out. If one is missing, update the GitHub connection's selected repositories and organization approval, then return to the picker.
3. Let Codex inspect the project and prepare its dependencies. Ask it to install the pinned SwarmOS version from this project's [README](../README.md#quickstart), plus Git, `gh`, and the coding harnesses your `.swarm/config.yaml` uses. Ensure these commands are on the task's `PATH`, not just available in the setup shell.
4. Review network access for the coordinator: it needs the Git remote and GitHub API, along with the model endpoints and package registries your chosen tools use. Check coordinator GitHub authentication and model authentication separately; connecting a repository does not guarantee that `gh` or a spawned coding CLI has credentials.
5. Test the setup, save it, and **Publish** the environment. When adding repositories or tools to an existing environment, edit and **Republish**, then start a new task; an existing task retains its own state.
6. Give your dot the environment name and the exact repository to use. OpenAI's Dot guide specifically requires creating the Codex environment before delegating a cloud task to it.

On the execution computer, also register each checkout with SwarmOS:

```bash
# Run inside each repository checked out in the cloud environment.
swarm doctor --backend github
swarm repo add --backend github
swarm repo list
```

If you are preparing an additional repository on a persistent host, SwarmOS can clone and register it there:

```bash
# Replace OWNER/REPO with a repository this host is authorized to use.
swarm repo add OWNER/REPO --clone --backend github
```

That command updates **SwarmOS's registry on that host**. It does not add a repository to your Codex Cloud environment or grant GitHub access. Do both steps when expanding the pool of repositories your dot can work on, and repeat registration in a new cloud workspace when needed. Do not bake live claim tokens or an authenticated operator's home directory into a shared environment snapshot.

For GitHub-triggered reviews and tasks, OpenAI currently documents the separate [Codex Cloud (Legacy) environment setup](https://learn.chatgpt.com/docs/environments/cloud-environment). Use that setup for the GitHub integration below rather than assuming every cloud environment is a GitHub review environment.

## Using your configured remote computer instead

If your repositories and tools already live on a development machine, use that host rather than rebuilding the setup in the cloud.

- **Dot access to your computer:** in the ChatGPT desktop app on that computer, open your dot's profile, find **Computers**, and choose **Allow access**. Keep the computer online with the app open. This is a separate permission from pairing a Codex remote connection.
- **Codex over SSH:** add the host to your SSH configuration, install and authenticate Codex there, then use **Settings → Connections** in the desktop app to add the SSH host and choose its remote project folders. OpenAI's [remote connections guide](https://learn.chatgpt.com/docs/remote-connections#connect-to-an-ssh-host) covers the setup.
- **SwarmOS on that host:** install `swarm` and the repository's configured tools there, then run the registration commands above for each checkout. A registry on your laptop is not automatically copied to the remote computer.

For Codex's own cross-host chat handoff, save a project for the same repository on each destination host. For a SwarmOS task handoff, use SwarmOS's published checkpoint and live claim rather than copying `.swarm/active/` or a local claim-token file between machines.

## Tell your dot how to work with the repository

Start with a conversation, not an adapter configuration:

> Use the Codex environment “MY-ENVIRONMENT” for OWNER/REPO. Read the README and `.swarm/` project guidance, look at issue #42, and tell me whether its objective and acceptance criteria are clear. Draft a plan or questions for me before starting implementation.

Once an authorized maintainer has approved the task, this is a useful handoff:

```text
Use the prepared Codex environment for OWNER/REPO and run SwarmOS there.
Check `swarm doctor --backend github`, then `swarm work 42 --dry-run`.
Show me any missing tools, permissions, credentials, or reviewer sandbox.
When execution is authorized, run `swarm work 42` with the repo's configured agents.
Read the task's WORKUNIT.md, SYSTEM_PROMPT.md, PLAN.md, and TEST_PLAN.md
from the worktree SwarmOS creates, not from another task or an old chat.
If the human gate pauses work, return the plan and wait for my decision.
After work and validation succeed, use `swarm submit 42` and send me the Draft PR.
Do not approve tasks, change policy, or merge without separate authorization.
```

The dot delegates to a Codex task; that task runs the SwarmOS coordinator with the prepared tools and access. This does not require the hosted dot itself to implement a local CLI contract. Let `swarm work` own the role sequence rather than manually editing coordination state or recursively starting it inside one of its agent roles.

For a prepared multi-repository host, you can ask it to run a bounded queue:

```bash
swarm node init --id dot-coordinator
swarm node run --all --concurrency 2 --max-jobs 4 --auto-submit
```

This runs the repository's configured coding agents, not an automatically discovered `dot` binary. A node drains and exits by default; add `--watch` only when you want continued polling. Keep the execution environment available for the duration of the run.

## Review PRs through Codex on GitHub

You can ask your dot to read a PR and discuss findings with you. For a review posted directly by Codex:

1. Connect the repository to Codex and configure its GitHub integration/environment.
2. In [Codex code review settings](https://chatgpt.com/settings/code-review), enable code review for that repository. Turn on automatic review if you want it on matching PR events.
3. Comment on a test PR:

```text
@codex review
```

For a narrower pass, use `@codex review for missing acceptance coverage`. Codex also supports follow-up task requests such as `@codex fix the CI failures`; these can change the branch when it has permission. See OpenAI's [GitHub review guide](https://learn.chatgpt.com/docs/third-party/github) for triggers and setup.

This is the Codex GitHub integration; you do not need to invite an account named `openai-dot`. If your organization provisions a separate specialist identity, use its actual verified account and normal repository permissions.

GitHub reviews are review input, not automatic SwarmOS plan approval. On a human-gated task, a maintainer evaluates the plan and findings, then records their own `swarm review 42 --approve` or `--reject`. On an agent-gated task, SwarmOS runs its configured sandboxed reviewer and records the exact plan-bound verdict. Ordinary comments do not substitute for either path.

## Running Codex directly in a SwarmOS role

You can also use the local Codex CLI without involving a hosted dot:

```yaml
agents:
  codex:
    binary: codex
    roles: [planner, developer, tester]
```

Merge this entry into the existing `.swarm/config.yaml`, configure the required model authentication, and have a maintainer commit the policy before task approval. For example, route planning and implementation to Codex while keeping a separately configured reviewer:

```bash
swarm work 42 --planner-agent codex --developer-agent codex
```

If you prefer a local policy name such as `dot`, set `binary: codex` and `harness: codex` explicitly under that name. This launches **Codex CLI**; the hosted Dot workflow uses the environment or computer connection described above.

Reviewer snapshots use a fresh home and explicit model-auth environment entries, so a normal interactive Codex login is not automatically inherited by a reviewer. Configure its sandbox, model authentication, and network access using the [integration runbook](developer.md#reviewer-isolation-and-credentials).

## Check the setup before leaving it running

First ask the dot to read the right repository, then complete one small approved task and inspect the plan, check output, and Draft PR. Confirm that a human gate actually pauses, that work appears in the expected environment or remote host, and that the chosen identity has only the repository access you intended.

These instructions describe the provider-supported setup and SwarmOS's current CLI. They do not configure your ChatGPT account, GitHub app installation, cloud environment, or remote computer automatically. Those connections stay under your control.

Related: [project setup](project.md) · [worker operation and recovery](developer.md) · [review gates](WORKFLOWS.md) · [security](../SECURITY.md).
