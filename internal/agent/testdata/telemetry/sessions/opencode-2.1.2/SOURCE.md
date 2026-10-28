# opencode 2.1.2 — unverified-shape

Hand-written to the OpenCode session-message shape (`info.tokens` with
`input`, `output`, `reasoning`, `cache.read`/`cache.write`, `info.modelID`, and
tool parts with `callID`, `tool` and `state.input`), wrapped in the `data`
envelope that the `api` command uses for `mcp.list` and `agent.get`. The
`session.create` and `session.messages` operation names are also unverified.
It is **not** captured output: opencode was not installed where this fixture
was written. Before merge, a maintainer must replace it with a redacted real
export saved during a run on the private server:

    opencode api --server <url> session.messages --param sessionID=<id>
