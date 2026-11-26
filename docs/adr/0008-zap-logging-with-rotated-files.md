# Logging goes through zap, to stdout and to a rotated JSON file

Trelis logs through `go.uber.org/zap`, wrapped in `internal/logger`, and writes every record twice: to stdout (console format in development, JSON in production) and to a JSON file rotated by lumberjack (`logs/trelis.log` in development, `/var/log/trelis/trelis.log` on a volume in the image). `env` (`development` or `production`) sets the defaults for level, format, file and caller; each `log.*` key still overrides its default. We chose zap over the standard library's `log/slog`, which the code used first, to use the logger most Go services in production run, with typed fields and its own sampling and stacktrace controls. We keep a file beside stdout so the logs of a run survive the process and can be read or handed over without a log collector, which the demo deployment does not have.

## Considered Options

- **Keep `log/slog` and add lumberjack as its writer.** Rejected: it would have needed the least code, but the user chose zap's API for the rest of the codebase.
- **stdout only, as 12-factor apps do, leaving collection to Docker or the platform.** Rejected for now: nothing collects it in the demo setup. stdout stays on, so a collector such as Loki can be added later without code changes.

## Consequences

- No code imports `log/slog` or `log`: `depguard` in golangci-lint rejects them outside tests. Temporal gets zap through a small adapter in `internal/logger` that implements Temporal's `log.Logger` on `zap.SugaredLogger`.
- Request fields travel in the context, not in function arguments. Middleware puts a child logger carrying `request_id` into the context, and code that has a ctx logs through `logger.Ctx(ctx)`, never `zap.L()`, or the record loses its request. Tracing (#74) adds `trace_id` and `span_id` to the same logger.
- The image runs distroless as `nonroot` with no writable directory, so the log file needs a mounted volume (`trelis-logs` in `compose.yaml`). Trelis refuses to start when it cannot open the log file, rather than silently logging to stdout only.
- Logs never carry secrets, the `Authorization` header, request or response bodies, Document or Chunk content, model prompts or completions, or Document file names. They carry IDs, because whoever reads the logs is not on a Restricted Document's Access List.
