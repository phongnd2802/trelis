## Workflow

Each ticket gets its own branch `<type>/<slug>` (e.g. `ci/build-green`, `feat/chunker-core`) cut from `main`, and a PR whose body has `Closes #N`. `main` only changes through merged PRs. Before pushing, run `make lint vet test` (CI also runs the `-linked` variants and a compose smoke test, see `.github/workflows/ci.yml`). For development, start the dependencies with `make dev-up` and run Trelis on the host with `make dev`; `make stack-up` starts the full stack with the trelis image (what CI smoke-tests); after pushing, wait for CI with `scripts/ci-wait.sh <branch>`.

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues on phongnd2802/trelis (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
