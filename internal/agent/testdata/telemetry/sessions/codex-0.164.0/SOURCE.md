# codex 0.164.0 — unverified-shape

Hand-written to the Codex rollout layout already parsed for #199
(`$CODEX_HOME|~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<session-id>.jsonl`,
`turn_context`, `response_item`, `event_msg` `token_count` with
`reasoning_output_tokens`). It is **not** captured output: codex was not
installed where this fixture was written. Before merge, a maintainer must
replace it with a redacted real log:

    codex exec --skip-git-repo-check "list files"
    # copy the rollout-*-<id>.jsonl named by the "session id:" stderr line

Two files are two `codex exec` turns, each named by its stderr session id.
