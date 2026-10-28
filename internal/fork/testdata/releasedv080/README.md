# Frozen v0.8.0 fork protocol

Commit: `58f33b8101acecb2c479e548addae8760a7e8733`. Origin metadata: {"Version":"v0.8.0","Time":"2026-10-28T18:28:01Z","Origin":{"VCS":"git","URL":"https://github.com/supernovae/swarmos","Hash":"58f33b8101acecb2c479e548addae8760a7e8733","Ref":"refs/tags/v0.8.0"}}

Module checksum: `h1:GuuWQB6xj0HZVS23ywm+dhSokMudyhObyIQJ5ZSUtqI=`.

Extracted from the exact cached Go module source. All five source inputs below were compared byte-for-byte with the module zip (SHA-256 `28ff9143ce436a2b03ee5ae780523f3e601bcd07ced81e98cf2154baf8ff2712`). Source file SHA-256 hashes:

- `internal/fork/transport.go`: `cd374095850125cf9efedb29e08979e43dc155001db9ea8ec942c73207f16def`
- `internal/lease/fork_request.go`: `eb6015549205e8b548957c2345c973f7a1ecab2d367d903517efa53071610946`
- `internal/lease/rebase.go`: `5a17627346f039c06c87b954244c2504221492d10b685d239c36963f601a307c`
- `internal/contract/types.go`: `602fe9e7470b36cf5f81e01f1d6964fb51b56a82be5d2ee20a0c3a93939a903a`
- `LICENSE`: `3fd611fbc34a3eedbc8e60db2d1fb4eea791a3952d11deb7ae5a97e348d393e4`

Symbol mapping: transport.go retains ForkResult, all four transport constants, ParseRequestComment, FormatResultComment, ParseResultComment, collapsedComment, commentPayload, resultSummary, collapsedPayload and operationName from internal/fork/transport.go.

Types from `internal/lease/fork_request.go`: ForkRequest.

Types from `internal/lease/rebase.go`: RebaseContext, RebasePlanReport, RebaseResolutionReport.

Types from `internal/contract/types.go`: GateMode, ArtifactType, Artifact.

Mechanical adaptations only: package renamed to releasedv080, imports reduced to the standard library, lease.ForkRequest and contract type qualifiers replaced with frozen local declarations, gofmt applied. Logic and JSON tags are unchanged. Only tests import this package. See [release preparation](../../../../docs/developer.md#changelog-fragments) for refresh instructions.
