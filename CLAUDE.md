## Workflow

Each ticket gets its own branch `<type>/<slug>` (e.g. `ci/build-green`, `feat/chunker-core`) cut from `main`, and a PR whose body has `Closes #N`. `main` only changes through merged PRs. Before pushing, run `make lint vet test` (CI also runs the `-linked` variants, see `.github/workflows/ci.yml`).

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues on phongnd2802/trelis (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
