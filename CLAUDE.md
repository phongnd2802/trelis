## Workflow

- Each ticket gets its own branch `<type>/<slug>` (e.g. `feat/chunker-core`) cut from `main`, and a PR whose body has `Closes #N`. `main` only changes through merged PRs.
- Before pushing, run `make lint vet test`; CI also runs the `-linked` variants, integration tests and a compose smoke test (`.github/workflows/ci.yml`). After pushing, wait with `scripts/ci-wait.sh <branch>`.
- Develop with `make dev-up` (dependencies) and `make dev` (Trelis on the host; Swagger UI at http://localhost:8080/swagger/index.html). `make stack-up` runs the full stack with the trelis image.
- Read `docs/feature-map.md` before exploring the code: it says what exists, where it lives and what is planned.
- `CODING_STANDARDS.md` holds the review rules (access, layers, logging, HTTP API, docs, tests).

## Agent skills

### Implementation (/implement)

When running `/implement` (`mattpocock-skills:implement`), follow that skill's steps but Qwen Code writes the code, not Claude:

- Claude fetches the ticket, reads the relevant code, and writes a brief for Qwen: goal, acceptance criteria, files and patterns to follow, the TDD seams to work through (test first, then code), and constraints (stay in scope, no new dependencies unless named, do not commit or push).
- Drive Qwen with `scripts/qwen.sh` (its header documents the commands): a `new` session per ticket, `send` the brief, `wait` for it. The user watches with `qwen attach --session <id>`; tell them the id. Send without asking the user first. If no daemon is running, ask the user to start `qwen serve`.
- Claude reviews: read the whole `git diff`, run `make lint vet test` itself, then run the `code-review` (`mattpocock-skills:code-review`) skill, whose Standards axis applies `CODING_STANDARDS.md`.
- Send findings back to the same session instead of fixing them yourself; only trivial edits (typos, imports) may be fixed directly. Stop and report to the user after 3 rounds that don't converge.
- When Qwen waits on a permission or a question, tell the user; they answer it.
- Once the review passes, commit as the skill says.

### Issue tracker

Issues are tracked in GitHub Issues on phongnd2802/trelis (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
