# claude 2.1.311 — unverified-shape

Hand-written to the publicly documented Claude Code project-log layout
(`~/.claude/projects/<slug of cwd>/<session-id>.jsonl`, assistant
`message.usage`). It is **not** captured output: claude was not installed where
this fixture was written. Before merge, a maintainer must replace it with a
redacted real log:

    cd "$(mktemp -d)" && claude --print --session-id "$(uuidgen)" "list files"
    # then copy ~/.claude/projects/<slug>/<id>.jsonl, replacing cwd with /CONTROL

Two files are two `--print` turns, each named by its `--session-id`.
