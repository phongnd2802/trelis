## Workflow

Each ticket gets its own branch `<type>/<slug>` (e.g. `ci/build-green`, `feat/chunker-core`) cut from `main`, and a PR whose body has `Closes #N`. `main` only changes through merged PRs. Before pushing, run `make lint vet test` (CI also runs the `-linked` variants and a compose smoke test, see `.github/workflows/ci.yml`). For development, start the dependencies with `make dev-up` and run Trelis on the host with `make dev`; `make stack-up` starts the full stack with the trelis image (what CI smoke-tests); after pushing, wait for CI with `scripts/ci-wait.sh <branch>`. A new or changed route needs swag annotations on its handler and `make swagger`; the dev Swagger UI is at http://localhost:8080/swagger/index.html. Log through `internal/logger`: `logger.Ctx(ctx)` whenever a ctx is at hand, never `zap.L()` there, and log an error once at the boundary (handler/middleware, Temporal activity) — inner layers wrap with `%w`, and only a swallowed error is logged where it happens. Never log secrets, bodies, Document/Chunk content, prompts/completions or Document file names.

`docs/feature-map.md` says what exists, where it lives and what is planned: read it before exploring the code, and a PR that adds, moves or changes the status of a feature updates it (`make lint` fails on a path it names that no longer exists).

## Agent skills

### Implementation (/implement)

When running `/implement` (`mattpocock-skills:implement`), follow that skill's steps but Qwen Code writes the code, not Claude:

- Claude fetches the ticket, reads the relevant code, and writes a brief for Qwen: goal, acceptance criteria, files and patterns to follow, the TDD seams to work through (test first, then code), and constraints (stay in scope, no new dependencies unless named, do not commit or push).
- Drive Qwen with `scripts/qwen.sh` (its header documents the commands): a `new` session per ticket, `send` the brief, `wait` for it. The user watches with `qwen attach --session <id>`; tell them the id. Send without asking the user first. If no daemon is running, ask the user to start `qwen serve`.
- Claude reviews: read the whole `git diff`, run `make lint vet test` itself, check that `docs/feature-map.md` reflects the change, then run the `code-review` (`mattpocock-skills:code-review`) skill.
- Send findings back to the same session instead of fixing them yourself; only trivial edits (typos, imports) may be fixed directly. Stop and report to the user after 3 rounds that don't converge.
- When Qwen waits on a permission or a question, tell the user; they answer it.
- Once the review passes, commit as the skill says.

### Issue tracker

Issues are tracked in GitHub Issues on phongnd2802/trelis (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
