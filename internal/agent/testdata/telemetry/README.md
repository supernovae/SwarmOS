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
