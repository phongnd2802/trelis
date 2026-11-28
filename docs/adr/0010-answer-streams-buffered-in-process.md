# Answer streams are buffered in process for resume

A streamed Pipeline Answer (#43) sends its tokens over Server-Sent Events while the chat role generates them. The client that asked may drop, so the Answer Events have to survive the request that produced them: `GET /answers/{answer_id}/events` with `Last-Event-ID` replays the later ones. The Answer Events live in an in-memory hub in the answer service, keyed by answer ID, owner and per-answer sequence number. Draining the open chat stream runs in a goroutine detached from the request's cancellation, bounded by five minutes and cancelled when the process shuts down.

We chose this because one Trelis process serves the stream, and a restart loses the generation anyway: a buffer that does not survive the process is no weaker than the work it holds. It also costs no write per token. A finished answer stays resumable for ten minutes; one answer keeps at most 10,000 Answer Events (terminal event included), and reaching that cap ends the stream with a generic error event rather than growing without bound.

Opening the chat stream happens before the first Answer Event, while the request can still answer JSON: a provider rejection there is the same 502 the JSON answer returns, not an error event after an SSE 200. Only a failure while draining the already-open stream becomes the terminal `error` Answer Event.

## Consequences

- Several replicas need sticky routing for a resume to reach the process that generated the answer, or the resuming request finds the generic not-found answer. The demo deployment runs one process, so this is not a current constraint.
- Memory per live or recently finished answer is bounded by the event cap; expiring finished answers after ten minutes bounds how many are retained.
- A failure while draining the stream is logged once where it becomes the terminal `error` event; token text, prompts and provider bodies never enter the log.
- The HTTP API's normal JSON answer is unchanged. Every failure before generation (authentication, validation, no `models.chat`, retrieval and opening the chat stream) is still the ordinary JSON error, sent before any SSE header.

## Considered Options

- **Store events in Postgres.** Rejected: one insert per token adds write load and latency to the path that exists to show tokens promptly, and the buffer only has to outlive the client's connection.
- **Replay through a shared cache (Redis).** Rejected: it adds a dependency for a buffer whose lifetime is already tied to this process's generation, and ADR-0005 keeps background work in Temporal, not another store.
- **No resume; retry the whole answer.** Rejected: a client that drops after the first token would pay for a second model call, and `Last-Event-ID` is already the stream contract the ingest progress endpoint uses.