# Synthetic session-log format samples

These files are **synthetic format samples** written by hand for parser tests.
They are not captured or qualified Claude Code or Codex CLI output and do not
establish compatibility with real harness logs. Field names follow the publicly
observed layouts (Claude Code `~/.claude/projects/**/*.jsonl` assistant `usage`
records; Codex `~/.codex/sessions/**/*.jsonl` `turn_context`, `response_item`
and `event_msg` `token_count` records). Verification against real logs, with
CLI versions, is on the PR review checklist.

Each driver directory holds the contents of the harness's session-log
directory (`claude/` mirrors `~/.claude/projects`, `codex/` mirrors
`~/.codex/sessions`). The leading dot-directory is omitted so the samples
survive exports that drop dot-directories; tests copy them into place under a
`t.TempDir()` scratch HOME.

`sessions/<harness>-<version>/` holds the host-HOME session records read for
isolated delegated and native runs (#238), one directory per harness version.
Each has a `SOURCE.md` saying whether its files were captured from that
version (`omp-18.12.6`) or are hand-written to its expected shape
(`unverified-shape`: `claude-2.1.311`, `codex-0.164.0`, `opencode-2.1.2`) and
must be replaced by a redacted real capture before merge. `/CONTROL` stands for
the run's control directory and is substituted by the tests.
