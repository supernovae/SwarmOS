# omp 18.12.6 — captured

Captured from the installed `omp/18.12.6` binary. It ran in a throwaway HOME,
with `--mode json -p --tools bash`, against a localhost fake
OpenAI-compatible endpoint that answered with two `bash {"command":"echo hi"}`
tool calls, then a final text reply. Each reply reported
`prompt_tokens=1002`, `cached_tokens=400`, `completion_tokens=22` and
`reasoning_tokens=7`. The file name and records are the real output; only the
session `cwd` was replaced with the `/CONTROL` placeholder, which tests
substitute. The same layout was confirmed for `--mode rpc` once `--no-session`
is dropped.

omp logs `input` without cache reads (602 = 1002 - 400). `model` is omp's
configured model id (`fake-model`); the provider's response model
(`fake-model-2026`) is not recorded.
