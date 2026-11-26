## Workflow

Each ticket gets its own branch `<type>/<slug>` (e.g. `ci/build-green`, `feat/chunker-core`) cut from `main`, and a PR whose body has `Closes #N`. `main` only changes through merged PRs. Before pushing, run `make lint vet test` (CI also runs the `-linked` variants and a compose smoke test, see `.github/workflows/ci.yml`). For development, start the dependencies with `make dev-up` and run Trelis on the host with `make dev`; `make stack-up` starts the full stack with the trelis image (what CI smoke-tests); after pushing, wait for CI with `scripts/ci-wait.sh <branch>`.

## Agent skills

### Implementation (/implement)

When running `/implement` (`mattpocock-skills:implement`), follow that skill's steps but Qwen Code writes the code, not Claude:

- Claude fetches the ticket, reads the relevant code, and writes a brief for Qwen: goal, acceptance criteria, files and patterns to follow, the TDD seams to work through (test first, then code), and constraints (stay in scope, no new dependencies unless named, do not commit or push).
- Run Qwen headless from the repo root with a fixed session id: `qwen --session-id "$SID" --approval-mode auto --max-wall-time 30m "<brief>"`. Never use `--yolo`.
- Claude reviews: read the whole `git diff`, run `make lint vet test` itself, then run the `code-review` skill.
- Send findings back to the same session (`qwen --resume "$SID" --approval-mode auto "<findings>"`) instead of fixing them yourself; only trivial edits (typos, imports) may be fixed directly. Stop and report to the user after 3 rounds that don't converge.
- Once the review passes, commit as the skill says.

### Issue tracker

Issues are tracked in GitHub Issues on phongnd2802/trelis (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
