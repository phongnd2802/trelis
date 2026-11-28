# Record and replay model calls

Trelis calls its model providers from inside the Go service, where the Python
harness cannot see them ([ADR-0001](../../docs/adr/0001-external-python-eval-harness.md)).
`trelis-eval proxy` sits between the two as an OpenAI-compatible provider: the
harness starts it, Trelis's per-role `base_url`s point at it, and paid calls
are recorded once and replayed for free afterwards.

The service has no response cache and no knowledge of the proxy. To Trelis it
is just another provider, so every request still goes through the real auth,
retrieval and SSE code paths; only the model is replaced. Replay is strict:
a request with no fixture fails instead of silently reaching a provider, which
is what makes a run repeatable and cost-free.

Terms are the ones in [GLOSSARY.md](../../GLOSSARY.md): Ablation Run,
Retrieval Config, Document, Chunk.

## Routing: one proxy, one path prefix per role

Roles can live on different providers (chat on one host, embeddings on
OpenRouter, for example), but a role's `base_url` can point at only one host.
The proxy therefore listens on one port and uses the first path segment as the
role:

```
TRELIS_MODELS_EMBEDDING_BASE_URL=http://127.0.0.1:4100/embedding
TRELIS_MODELS_CHAT_BASE_URL=http://127.0.0.1:4100/chat
TRELIS_EVAL_JUDGE_BASE_URL=http://127.0.0.1:4100/judge
```

The judge is not a Trelis model role: `trelis-eval answer run` calls it from
the harness. It is routed like a role anyway, because the proxy accepts any
name matching `[a-z]+`, and `judge` needed no proxy change. Trelis posts to
`{base_url}/embeddings`, `{base_url}/chat/completions`, `{base_url}/responses`
or `{base_url}/rerank`; the harness posts to
`{judge_base_url}/chat/completions`. The proxy resolves
`POST /<role>/<rest>` to `<upstream for role>/<rest>`, so
`/embedding/embeddings` goes to `https://openrouter.ai/api/v1/embeddings`
when `embedding` is configured with that URL, and `/judge/chat/completions`
goes to the judge provider.

The forwarding URL is `<upstream>/<rest>` with the upstream's trailing slash
removed. A query string on the incoming request is kept, forwarded and stored
in the fixture; model endpoints normally carry their parameters in the body.

Only `POST` is proxied; any other method answers `405` with `Allow: POST`. An
unknown role answers `404` with a JSON error naming the role and the roles the
proxy knows. In `record` and `passthrough` those are the roles named by
`--upstream`; in `replay` the proxy has no upstreams, so any lower-case role
is routed and an unrecorded one becomes a replay miss instead.

### Headers

The upstream request carries the incoming `Authorization`, `Content-Type` and
`Accept` unchanged. Hop-by-hop headers (`Connection`, `Transfer-Encoding`,
`Keep-Alive` and the like) are not forwarded. The proxy adds
`Accept-Encoding: identity` as a hint, then reads the answer with `iter_bytes`
and stores the decoded body, so a provider that compresses anyway still
records a plain answer.

The response keeps every upstream header except hop-by-hop ones
(`Connection`, `Keep-Alive`, `Transfer-Encoding`, `TE`, `Trailer`, `Upgrade`,
`Proxy-*`), `Content-Length` (the proxy sets the decoded length) and
`Content-Encoding` (the body is stored decoded). This happens in `record` and
`passthrough`, for a `2xx` and a failure alike, so a `429`'s `Retry-After` and
rate-limit headers reach Trelis and its retry backoff sees them. A fixture
stores only the content type; replayed answers are `2xx` and need no other
header.

`--upstream` refuses a URL with userinfo (`https://user:key@host`): pass a
role's key through Trelis's `TRELIS_MODELS_<ROLE>_API_KEY` instead, where the
proxy forwards it in `Authorization` without ever putting it in a URL.

In `replay` no upstream is called, so no provider key is needed and Trelis's
own key value is irrelevant.

### The judge

`trelis-eval answer run` uses a judge model for the correctness Grader. Its
configuration is separate from Trelis's `models.chat`: the harness reads
`TRELIS_EVAL_JUDGE_BASE_URL` (or `--judge-base-url`), `TRELIS_EVAL_JUDGE_MODEL`
(or `--judge-model`) and `TRELIS_EVAL_JUDGE_API_KEY`. Route the judge through
the same proxy as the embedding role:

```sh
uv run --project eval trelis-eval proxy --mode record \
  --upstream embedding=https://openrouter.ai/api/v1 \
  --upstream judge=https://openrouter.ai/api/v1 \
  --fixtures eval/fixtures/models
```

Set `TRELIS_EVAL_JUDGE_BASE_URL=http://127.0.0.1:4100/judge` for the harness
in both modes. The proxy's startup hint prints
`TRELIS_MODELS_<ROLE>_BASE_URL` lines for Trelis's own roles, including a
`TRELIS_MODELS_JUDGE_BASE_URL` for the service's judge role; the harness's
judge is not that role, so set `TRELIS_EVAL_JUDGE_BASE_URL` to the same URL
instead. The judge's Chat Completions body (`model`, `messages`,
`temperature: 0`) is keyed like any other fixture, so a changed prompt or
judge model is a replay miss rather than a silently different verdict. In
`replay` the harness still reads `TRELIS_EVAL_JUDGE_API_KEY` and sends it as
`Authorization`, because it always sends one; with no upstream the proxy never
uses the value, so any non-empty value works.

## The key

`fixture_key(role, path, body)` is the hex SHA-256 of the role, the path after
the role and the normalised request body, framed with newlines:

```
role + "\n" + path + "\n" + normalise(body)
```

A JSON body is re-serialised with
`json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False)`,
so object key order and insignificant whitespace do not change the key while
any value does. A body that is not JSON is hashed as the raw bytes. Non-ASCII
text stays UTF-8 rather than `\u` escapes, which keeps the stored request
readable.

Headers, including `Authorization`, are never part of the key and never
stored. Two callers with different keys but the same request share a fixture,
which is correct: the provider's answer is what is being recorded.

## The fixture store

```
<fixtures-dir>/<role>/<key[:2]>/<key>.json
```

The two-character directory keeps thousands of fixtures in manageable
directories. The key appears there and inside the file so a fixture can be
found and audited by hand; the provider API key does not appear anywhere.

```json
{
  "schema_version": 1,
  "key": "9f2c…",
  "role": "embedding",
  "path": "/embeddings",
  "request": {
    "input": ["…"],
    "model": "text-embedding-3-small"
  },
  "response": {
    "status": 200,
    "content_type": "application/json",
    "stream": false,
    "chunks_b64": ["eyJvYmplY3QiOiJsaXN0Ii…"]
  },
  "recorded_at": "2026-11-27T10:15:00Z"
}
```

- `request` is the normalised JSON body, so a developer can see what the
  fixture answers, including prompts and Chunk text. Fixtures are eval data,
  not service logs. A non-JSON body (never sent by the OpenAI-compatible
  endpoints) is kept as `{"raw_b64": "…"}`.
- `response.chunks_b64` holds the bytes of the upstream answer: one entry for
  a plain answer, and the chunks the proxy read for a streamed one
  (`text/event-stream`, or a request with `"stream": true`). A streamed answer
  keeps one entry per read, in order. TCP may merge the provider's separate
  writes, so the entries need not match those writes one-for-one; the
  concatenation and its order are exact, which is what an SSE consumer needs.
  Base64 keeps every byte, so the `usage` field and any provider-specific
  wording survive replay unchanged. Cost reporting that reads `usage`
  therefore stays real.
- `status` and `content_type` are replayed as recorded. Only `2xx` answers are
  saved; a `429`, `500` or any other failure is passed back to Trelis
  untouched and never becomes a fixture. Trelis retries `429` and `5xx` and
  nothing else, so a transient provider failure stays transient.

Writes are atomic: the file is written to a temporary name in the same
directory and `os.replace`d into place, so a threaded proxy never serves a
half-written fixture and no `.tmp` file is left behind.

The proxy builds one `httpx.Client` (300 s timeout) and streams upstream with
`client.stream(...)`. A streamed answer is forwarded chunk by chunk with a
flush after each one, framed with `Transfer-Encoding: chunked`; a plain answer
gets a `Content-Length`. Replay writes the stored chunks in the same order,
flushing between them.

## Modes

- **`record`** — if the fixture exists, serve it; otherwise call the upstream,
  stream the answer back to Trelis as it arrives, and save it on a `2xx`.
  Recording is idempotent: re-running the matrix after a prompt change pays
  only for the requests whose keys changed. There is no in-flight dedupe: two
  identical requests racing in `record` both reach the provider, and the
  second save replaces the first with equal bytes.
- **`replay`** — fixtures only, no upstream and no key. A missing fixture
  answers `404` (non-retryable for Trelis, which retries `429` and `5xx`) with
  this body:

  ```json
  {
    "error": {
      "message": "replay miss: no fixture for embedding /embeddings (key 9f2c…); re-record with --mode record",
      "type": "replay_miss",
      "key": "9f2c…"
    }
  }
  ```

  Each miss also prints one line to stderr naming the role, path, key and the
  request's `model`, and appends `{recorded_at, key, role, path, request}` to
  `<fixtures-dir>/misses.jsonl`.
- **`passthrough`** — forward and stream back, save nothing, never read
  fixtures. Use it to measure real provider latency without touching the
  fixture tree.

## Running the proxy

```sh
uv run --project eval trelis-eval proxy \
  --mode record \
  --upstream embedding=https://openrouter.ai/api/v1 \
  --fixtures eval/fixtures/models \
  --listen 127.0.0.1:4100
```

`--upstream ROLE=URL` is repeatable. A role is `[a-z]+`, may be named once,
and its URL must be an http(s) base with no query or fragment (the request
path is appended to it; a query is also a place for a key to leak). `--mode`
is required and is one of `record`, `replay`, `passthrough`. `--fixtures`
defaults to `eval/fixtures/models` and `--listen` to `127.0.0.1:4100`.

After binding, the command prints the listen address and one
`TRELIS_MODELS_<ROLE>_BASE_URL=...` line per role to copy into the shell that
starts Trelis, then serves until Ctrl-C. In `replay` without `--upstream`, the
roles are the ones already present in the fixture tree (or any role it is
asked for). `eval/fixtures/` is gitignored for now; committed fixtures come
with the CI smoke eval (#53).

`trelis-eval proxy fixtures --fixtures <dir>` prints the tree without
starting the proxy: one line per role and request model with its fixture
count, and `(no model)` for a fixture whose request names none. The model is
part of the key, so this shows what a changed model would cost;
`scripts/eval-stack.sh up` calls it before starting Trelis for that reason.

### Worked example: the dense baseline

Ingest and the Golden Set come first, as in [the README](../README.md). In one
terminal, from the repository root:

```sh
make dev-up

# Record: the first run of the matrix pays for every embedding call.
uv run --project eval trelis-eval proxy --mode record \
  --upstream embedding=https://openrouter.ai/api/v1 \
  --fixtures eval/fixtures/models
```

The proxy prints:

```
Listening on http://127.0.0.1:4100/ (mode: record)
Point Trelis's model roles at the proxy with:
  TRELIS_MODELS_EMBEDDING_BASE_URL=http://127.0.0.1:4100/embedding
Press Ctrl-C to stop.
```

In a second terminal, start Trelis with that variable and run the dense
baseline. The paths below are the stratified-500 baseline's files, the same
ones [the baseline notes](../baselines/README.md) use; substitute whatever your
own ingest wrote. The embedding model name and API key still come from
`.local/models.env`; the proxy forwards the key upstream. Note that `make dev`
sources `.local/models.env` *after* the inline variable, so a
`TRELIS_MODELS_EMBEDDING_BASE_URL` line in that file would silently override
the proxy URL: check it and update or remove that line while recording and
replaying.

```sh
TRELIS_MODELS_EMBEDDING_BASE_URL=http://127.0.0.1:4100/embedding make dev

uv run --project eval trelis-eval retrieval run \
  --golden eval/data/multihop-rag/strat500-golden.jsonl \
  --manifest eval/data/multihop-rag/strat500-manifest.jsonl \
  --run-id before-cache
```

Stop Trelis and the proxy. Start the proxy again in `replay`; no upstream and
no provider key are needed:

```sh
uv run --project eval trelis-eval proxy --mode replay \
  --fixtures eval/fixtures/models
```

Start Trelis with the same `TRELIS_MODELS_EMBEDDING_BASE_URL` and run the same
command under a new `--run-id` (the command refuses to overwrite a results
file):

```sh
uv run --project eval trelis-eval retrieval run \
  --golden eval/data/multihop-rag/strat500-golden.jsonl \
  --manifest eval/data/multihop-rag/strat500-manifest.jsonl \
  --run-id after-cache
```

Every embedding answer now comes from `eval/fixtures/models`, byte for byte,
and the second run costs nothing. Results from the two runs are comparable
because the model's answer, including `usage`, is the one the first run paid
for.

### Ingest embeds Chunks too

Ingest uses the same embedding role to embed every Chunk, so an ingest run
under `record` records Chunk embeddings as well as query embeddings.
Replaying an ingest only works while the chunker's output is unchanged: if
chunking changes, the Chunk texts (and therefore the keys) change, and replay
reports misses. Record again before trusting the new ingest.

## When replay misses

A miss means the request changed — a prompt, the retrieved Chunks, the model
name or a parameter. The body already names the role, path and key; the
stderr line adds the `model`, and `misses.jsonl` keeps the normalised request
so the difference can be diffed against the recorded one.

To fix it, stop the failing replay and start `record` again with the same
`--upstream` and `--fixtures` values, then rerun the failing step. Existing
fixtures are still served from disk; only the new requests reach the provider.
Recording writes the new fixtures next to the old ones, and the next `replay`
run covers both.