# Test fixtures

The `*.jsonl` files are real output from the pinned CLIs in `Dockerfile.agent`
(Claude Code 2.1.317, Codex 0.166.0, Antigravity 1.5.0), run against
`fakemodel/` rather than a real model. The event shapes are real. The model
behaviour, token counts and error wording from a live API are not, except for
`agy_fakekey.jsonl`, which holds Google's real response to an invalid key.
