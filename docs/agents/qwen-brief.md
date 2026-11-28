# Standing rules for a Qwen brief

`scripts/qwen.sh send` puts this file in front of every prompt. A ticket brief or a review round then only says what is specific to it.

## Every change

- Stay in the scope of the brief. Add a dependency only when the brief names it.
- Write each test before its code and watch it fail first.
- Scratch files and fake servers go in `.scratch/` inside the repo. A smoke test against the dev stack runs in its own `scripts/smoke-env.sh` instance. Call no real model and no running Trelis unless the brief asks for it.
- Leave `.local/` as it is.
- Leave committing and pushing to Claude: finish with the working tree changed and unstaged.

## Before you reply

Run the checks for what you changed, and report them as passing:

- Go: `make lint vet test`, plus `make test-integration` when `internal/integration/` changed.
- `eval/`: `make eval-lint eval-test` and `make lint`. When a report render changes on purpose, rewrite the golden files with `TRELIS_EVAL_UPDATE_GOLDEN=1 make eval-test` and say so.

Reply with a short summary: the files changed, the tests added, and anything you were unsure of.
