## Workflow

Each ticket gets its own branch `<type>/<slug>` (e.g. `ci/build-green`, `feat/chunker-core`) cut from `main`, and a PR whose body has `Closes #N`. `main` only changes through merged PRs. Before pushing, run `make lint vet test` (CI also runs the `-linked` variants and a compose smoke test, see `.github/workflows/ci.yml`). For development, start the dependencies with `make dev-up` and run Trelis on the host with `make dev`; `make stack-up` starts the full stack with the trelis image (what CI smoke-tests); after pushing, wait for CI with `scripts/ci-wait.sh <branch>`.

## Agent skills

### Implementation (/implement)

When running `/implement` (`mattpocock-skills:implement`), follow that skill's steps but Qwen Code writes the code, not Claude:

- Claude fetches the ticket, reads the relevant code, and writes a brief for Qwen: goal, acceptance criteria, files and patterns to follow, the TDD seams to work through (test first, then code), and constraints (stay in scope, no new dependencies unless named, do not commit or push).
- Drive Qwen through the user's `qwen serve` daemon (see below) so the user can watch and step in. Approval mode is `auto`; never `yolo`.
- Claude reviews: read the whole `git diff`, run `make lint vet test` itself, then run the `code-review` skill.
- Send findings back as a new prompt to the same daemon session instead of fixing them yourself; only trivial edits (typos, imports) may be fixed directly. Stop and report to the user after 3 rounds that don't converge.
- Once the review passes, commit as the skill says.

#### Calling Qwen through the daemon

The daemon listens on `http://127.0.0.1:4170` (loopback, no token). Don't send `X-Qwen-Client-Id`: an unregistered id is rejected.

1. Check `qwen daemon status` lists a running daemon for this repo. If none, ask the user to run `qwen serve` in the repo; don't start it yourself.
2. Start a **new session for every new ticket**: `POST /session` with `{"cwd": "<repo root>", "sessionScope": "thread"}` returns a fresh `sessionId` (without `sessionScope` the daemon hands back the workspace's shared session). Keep that id for the ticket's fix rounds; don't reuse it for another ticket. Set `POST /session/$ID/approval-mode` `{"mode":"auto"}`. Tell the user the id so they can watch with `qwen attach --session $ID` or the Web Shell at `http://127.0.0.1:4170/`.
3. Before every prompt, check `GET /session/$ID/status` is idle (`hasActivePrompt: false`, `pendingPromptCount: 0`), then send it straight away without asking the user. If the session is busy, wait for it to go idle.
4. Send the brief or the findings: build `{"prompt":[{"type":"text","text":"..."}]}` with `jq --rawfile` and `POST /session/$ID/prompt`. It returns at once with a `promptId`.
5. Wait with Monitor until status shows no active or pending prompt. If `isWaitingForPermission` or `isWaitingForUserQuestion` is true, tell the user. Never vote on a permission yourself. To stop a run: `POST /session/$ID/cancel`.
6. If the daemon isn't available and the user doesn't want to start it, fall back to headless: `qwen --session-id "$SID" --approval-mode auto --max-wall-time 30m "<brief>"`, then `qwen --resume "$SID" ...` for the fix rounds.

### Issue tracker

Issues are tracked in GitHub Issues on phongnd2802/trelis (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
