# Synthetic session-log format samples

These files are **synthetic format samples** written by hand for parser tests.
They are not captured or qualified Claude Code or Codex CLI output and do not
establish compatibility with real harness logs. Field names follow the publicly
observed layouts (Claude Code `~/.claude/projects/**/*.jsonl` assistant `usage`
records; Codex `~/.codex/sessions/**/*.jsonl` `turn_context`, `response_item`
and `event_msg` `token_count` records). Verification against real logs, with
CLI versions, is on the PR review checklist.
