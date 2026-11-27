"""Run one Ablation Run and read/write its versioned JSONL.

An Ablation Run scores one Retrieval Config against a Golden Set through
``POST /retrieve``. Relevance is at Document level; ``eval/docs/metrics.md``
holds the formulas and the schema.
"""

from __future__ import annotations

import hashlib
import json
import time
from collections.abc import Callable, Sequence
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import UTC, datetime
from enum import StrEnum
from importlib.metadata import version
from pathlib import Path
from typing import Any

import httpx

from ._fields import (
    check_fields,
    float_field,
    float_map_field,
    int_field,
    optional_float_field,
    optional_int_field,
    optional_string_field,
    string_field,
    string_list_field,
)
from .client import TraceStep, TrelisClient, TrelisError
from .golden import GoldenQuestion
from .metrics import (
    mean,
    ndcg_at_k,
    nearest_rank_percentile,
    recall_at_k,
    reciprocal_rank,
)

SCHEMA_VERSION = 1

MAX_CONCURRENCY = 32
"""The most questions one Ablation Run keeps in flight at once."""
HARNESS_VERSION = version("trelis-eval")

RECALL_5 = "recall@5"
RECALL_10 = "recall@10"
MRR = "mrr"
NDCG_10 = "ndcg@10"
METRIC_NAMES = (RECALL_5, RECALL_10, MRR, NDCG_10)


class QuestionStatus(StrEnum):
    """The outcome of asking one Golden question under one Retrieval Config."""

    SCORED = "scored"
    SKIPPED = "skipped"
    ERROR = "error"


class AblationAborted(RuntimeError):
    """A run stopped because Trelis rejected the Retrieval Config."""

    def __init__(self, status: int, message: str):
        self.status = status
        self.message = message
        super().__init__(message)


@dataclass(frozen=True)
class RetrievalConfig:
    """A named combination of Retrieval Techniques switched on together."""

    name: str
    techniques: tuple[str, ...]


def parse_retrieval_config(value: str) -> RetrievalConfig:
    """Parse ``techniques joined with '+'`` into a canonical config."""
    parts = tuple(part.strip() for part in value.split("+"))
    if not value.strip() or any(not part for part in parts):
        raise ValueError(
            f"invalid retrieval config {value!r}: name techniques joined with '+' "
            "(for example 'dense' or 'bm25+dense')"
        )
    if len(set(parts)) != len(parts):
        raise ValueError(f"invalid retrieval config {value!r}: a technique is listed twice")
    techniques = tuple(sorted(parts))
    return RetrievalConfig(name="+".join(techniques), techniques=techniques)


@dataclass(frozen=True)
class Manifest:
    """The completed Documents of an ingest manifest, both ways round."""

    source_by_document: dict[str, str]
    completed_source_ids: frozenset[str]


def load_manifest(path: str | Path) -> Manifest:
    """Load the JSONL written by ``write_manifest``, keeping completed records."""
    path = Path(path)
    source_by_document: dict[str, str] = {}
    completed_source_ids: set[str] = set()

    for line_number, raw_line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        where = f"{path}:{line_number}"
        if not raw_line.strip():
            raise ValueError(f"{where}: blank line (one JSON object per line)")
        try:
            value = json.loads(raw_line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"{where}: invalid JSON: {exc.msg}") from exc
        if not isinstance(value, dict):
            raise ValueError(f"{where}: expected a JSON object")

        source_id = value.get("source_id")
        if not isinstance(source_id, str) or not source_id.strip():
            raise ValueError(f"{where}: field 'source_id' must be a non-empty string")
        document_id = value.get("document_id")
        if document_id is not None and not isinstance(document_id, str):
            raise ValueError(f"{where}: field 'document_id' must be a string or null")
        status = value.get("status")
        if not isinstance(status, str) or not status.strip():
            raise ValueError(f"{where}: field 'status' must be a non-empty string")
        if status != "completed":
            continue
        if not isinstance(document_id, str) or not document_id.strip():
            raise ValueError(f"{where}: completed record needs a non-empty 'document_id'")

        earlier = source_by_document.get(document_id)
        if earlier is not None and earlier != source_id:
            raise ValueError(
                f"{where}: document_id {document_id!r} maps to both {earlier!r} and {source_id!r}"
            )
        source_by_document[document_id] = source_id
        completed_source_ids.add(source_id)

    return Manifest(
        source_by_document=source_by_document,
        completed_source_ids=frozenset(completed_source_ids),
    )


@dataclass(frozen=True)
class RetrievedRecord:
    """One retrieved Chunk in a results file, with the Document it maps to."""

    chunk_id: str
    document_id: str
    source_id: str | None
    score: float
    scores: dict[str, float]


@dataclass(frozen=True)
class QuestionMetrics:
    """The four retrieval metrics of one scored Golden question."""

    recall_at_5: float
    recall_at_10: float
    mrr: float
    ndcg_at_10: float

    def to_json(self) -> dict[str, float]:
        """Return the metric names and values written to JSONL."""
        return {
            RECALL_5: self.recall_at_5,
            RECALL_10: self.recall_at_10,
            MRR: self.mrr,
            NDCG_10: self.ndcg_at_10,
        }


@dataclass(frozen=True)
class QuestionResult:
    """The outcome of asking one Golden question under one Retrieval Config."""

    id: str
    slices: tuple[str, ...]
    status: QuestionStatus
    evidence: tuple[str, ...]
    retrieved: tuple[RetrievedRecord, ...]
    trace: tuple[TraceStep, ...]
    metrics: QuestionMetrics | None
    latency_ms: float
    error: str | None = None


@dataclass(frozen=True)
class RunSummary:
    """Means over the scored questions, counts, and scored-call latencies."""

    recall_at_5: float | None
    recall_at_10: float | None
    mrr: float | None
    ndcg_at_10: float | None
    scored: int
    skipped: int
    error: int
    latency_p50: float | None
    latency_p95: float | None

    @property
    def metrics(self) -> dict[str, float | None]:
        """Return the metric names and means written to JSONL."""
        return {
            RECALL_5: self.recall_at_5,
            RECALL_10: self.recall_at_10,
            MRR: self.mrr,
            NDCG_10: self.ndcg_at_10,
        }

    @property
    def counts(self) -> dict[str, int]:
        """Return the question counts written to JSONL."""
        return {
            QuestionStatus.SCORED.value: self.scored,
            QuestionStatus.SKIPPED.value: self.skipped,
            QuestionStatus.ERROR.value: self.error,
        }


@dataclass(frozen=True)
class AblationRun:
    """One Golden Set scored under one Retrieval Config."""

    config: RetrievalConfig
    top_k: int
    started_at: str
    finished_at: str
    questions: tuple[QuestionResult, ...]
    summary: RunSummary
    # Requests in flight at once. Latency is measured under this load, so two
    # runs' latencies compare only at the same concurrency.
    concurrency: int = 1


def run_ablation(
    client: TrelisClient,
    golden: Sequence[GoldenQuestion],
    manifest: Manifest,
    config: RetrievalConfig,
    top_k: int = 10,
    *,
    clock: Callable[[], float] = time.monotonic,
    concurrency: int = 1,
) -> AblationRun:
    """Score every Golden question under one Retrieval Config.

    Questions with no evidence are skipped, and an ordinary request error is
    recorded on its question and excluded from the means. A 400 or 503 raises
    :class:`AblationAborted`, because Trelis rejected the config rather than the
    question. Missing evidence Documents and a per-Principal question fail
    before any request runs.

    With ``concurrency`` above 1, that many questions are in flight at once;
    the results keep Golden Set order, and an abort cancels the questions not
    yet sent.
    """
    if (
        not isinstance(concurrency, int)
        or isinstance(concurrency, bool)
        or not 1 <= concurrency <= MAX_CONCURRENCY
    ):
        raise ValueError(
            f"concurrency must be between 1 and {MAX_CONCURRENCY}, got {concurrency!r}"
        )
    if not isinstance(top_k, int) or isinstance(top_k, bool) or not 10 <= top_k <= 100:
        raise ValueError(f"top_k must be between 10 and 100, got {top_k!r}")
    _check_principals(golden)
    _check_evidence(golden, manifest)

    started_at = _utc_now()
    results = _score_questions(client, golden, manifest, config, top_k, clock, concurrency)
    finished_at = _utc_now()

    scored = [result for result in results if result.status is QuestionStatus.SCORED]
    scored_metrics = [result.metrics for result in scored if result.metrics is not None]
    latencies = [result.latency_ms for result in scored]
    summary = RunSummary(
        recall_at_5=mean([metrics.recall_at_5 for metrics in scored_metrics]),
        recall_at_10=mean([metrics.recall_at_10 for metrics in scored_metrics]),
        mrr=mean([metrics.mrr for metrics in scored_metrics]),
        ndcg_at_10=mean([metrics.ndcg_at_10 for metrics in scored_metrics]),
        scored=len(scored),
        skipped=sum(1 for result in results if result.status is QuestionStatus.SKIPPED),
        error=sum(1 for result in results if result.status is QuestionStatus.ERROR),
        latency_p50=nearest_rank_percentile(latencies, 50),
        latency_p95=nearest_rank_percentile(latencies, 95),
    )
    return AblationRun(
        config=config,
        top_k=top_k,
        started_at=started_at,
        finished_at=finished_at,
        questions=results,
        summary=summary,
        concurrency=concurrency,
    )


def step_latencies(run: AblationRun) -> dict[str, tuple[int, int]]:
    """Return each trace step's p50 and p95 duration in ms, keyed ``technique/step``.

    The request latency alone hides which step is slow: the first dense
    baseline looked bound by the embedding provider while its search step
    was the one taking 20 s (#143). Only scored questions count, like the
    request latencies; keys are sorted so the printout is stable.
    """
    durations: dict[str, list[int]] = {}
    for result in run.questions:
        if result.status is not QuestionStatus.SCORED:
            continue
        for step in result.trace:
            name = f"{step.technique}/{step.step}" if step.technique else step.step
            durations.setdefault(name, []).append(step.duration_ms)
    return {
        name: (nearest_rank_percentile(values, 50), nearest_rank_percentile(values, 95))
        for name, values in sorted(durations.items())
    }


def _score_questions(
    client: TrelisClient,
    golden: Sequence[GoldenQuestion],
    manifest: Manifest,
    config: RetrievalConfig,
    top_k: int,
    clock: Callable[[], float],
    concurrency: int,
) -> tuple[QuestionResult, ...]:
    """Score the questions, ``concurrency`` at a time, in Golden Set order."""
    if concurrency == 1:
        return tuple(
            _score_question(client, question, manifest, config, top_k, clock) for question in golden
        )
    executor = ThreadPoolExecutor(max_workers=concurrency)
    futures = [
        executor.submit(_score_question, client, question, manifest, config, top_k, clock)
        for question in golden
    ]
    try:
        return tuple(future.result() for future in futures)
    finally:
        # On an abort, drop the questions still queued instead of sending
        # each one only to be rejected the same way.
        executor.shutdown(wait=True, cancel_futures=True)


def _check_principals(golden: Sequence[GoldenQuestion]) -> None:
    """Reject questions asked as a named Principal before any request runs."""
    named = [question for question in golden if question.principal is not None]
    if not named:
        return
    listed = ", ".join(f"{question.id!r} ({question.principal!r})" for question in named)
    raise ValueError(
        "the Golden Set asks questions as a named Principal, which per-Principal "
        f"Ablation Runs do not support yet: {listed}. The run acts as the "
        "TRELIS_API_KEY Principal."
    )


def _check_evidence(golden: Sequence[GoldenQuestion], manifest: Manifest) -> None:
    """Reject evidence without a completed manifest record before any request."""
    missing = sorted(
        {
            evidence.source_id
            for question in golden
            for evidence in question.evidence
            if evidence.source_id not in manifest.completed_source_ids
        }
    )
    if missing:
        raise ValueError(
            "the Golden Set cites evidence with no completed manifest record: " + ", ".join(missing)
        )


def _score_question(
    client: TrelisClient,
    question: GoldenQuestion,
    manifest: Manifest,
    config: RetrievalConfig,
    top_k: int,
    clock: Callable[[], float],
) -> QuestionResult:
    """Ask one question, or skip it when it has no evidence to score against."""
    evidence = tuple(dict.fromkeys(item.source_id for item in question.evidence))
    if not evidence:
        return QuestionResult(
            id=question.id,
            slices=question.slices,
            status=QuestionStatus.SKIPPED,
            evidence=(),
            retrieved=(),
            trace=(),
            metrics=None,
            latency_ms=0.0,
        )

    started = clock()
    try:
        answer = client.retrieve(question.question, top_k, config.techniques)
    except TrelisError as exc:
        if exc.status in (400, 503):
            raise AblationAborted(exc.status, str(exc)) from exc
        return QuestionResult(
            id=question.id,
            slices=question.slices,
            status=QuestionStatus.ERROR,
            evidence=evidence,
            retrieved=(),
            trace=(),
            metrics=None,
            latency_ms=_elapsed_ms(started, clock()),
            error=str(exc),
        )
    except httpx.HTTPError as exc:
        return QuestionResult(
            id=question.id,
            slices=question.slices,
            status=QuestionStatus.ERROR,
            evidence=evidence,
            retrieved=(),
            trace=(),
            metrics=None,
            latency_ms=_elapsed_ms(started, clock()),
            error=str(exc),
        )
    latency_ms = _elapsed_ms(started, clock())

    retrieved = tuple(
        RetrievedRecord(
            chunk_id=result.chunk_id,
            document_id=result.document_id,
            source_id=manifest.source_by_document.get(result.document_id),
            score=result.score,
            scores=dict(result.scores),
        )
        for result in answer.results
    )
    ranked = [record.source_id for record in retrieved]
    relevant = set(evidence)
    metrics = QuestionMetrics(
        recall_at_5=recall_at_k(ranked, relevant, 5),
        recall_at_10=recall_at_k(ranked, relevant, 10),
        mrr=reciprocal_rank(ranked, relevant),
        ndcg_at_10=ndcg_at_k(ranked, relevant, 10),
    )
    return QuestionResult(
        id=question.id,
        slices=question.slices,
        status=QuestionStatus.SCORED,
        evidence=evidence,
        retrieved=retrieved,
        trace=answer.trace,
        metrics=metrics,
        latency_ms=latency_ms,
    )


def _elapsed_ms(started: float, finished: float) -> float:
    """Turn two clock readings in seconds into milliseconds."""
    return round((finished - started) * 1000.0, 3)


def _utc_now() -> str:
    """Return the current UTC time as an ISO 8601 string."""
    return datetime.now(UTC).isoformat()


def default_run_id(*, now: Callable[[], datetime] | None = None) -> str:
    """Return ``YYYYMMDDTHHMMSSZ`` in UTC, injectable for tests."""
    current = now() if now is not None else datetime.now(UTC)
    if current.tzinfo is None:
        current = current.replace(tzinfo=UTC)
    return current.astimezone(UTC).strftime("%Y%m%dT%H%M%SZ")


def file_sha256(path: str | Path) -> str:
    """Return the SHA-256 of a file's bytes."""
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        for block in iter(lambda: source.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


@dataclass(frozen=True)
class GoldenSetRef:
    """The Golden Set a run was scored against, with its digest."""

    path: str
    sha256: str
    questions: int


@dataclass(frozen=True)
class ManifestRef:
    """The ingest manifest a run used, with its digest."""

    path: str
    sha256: str


@dataclass(frozen=True)
class _RunHeader:
    """The parsed fields of a run header, before the questions and summary."""

    run_id: str
    config: RetrievalConfig
    top_k: int
    golden_set: GoldenSetRef
    manifest: ManifestRef
    base_url: str
    harness_version: str
    started_at: str
    concurrency: int


@dataclass(frozen=True)
class AblationRunFile:
    """The three record types of one Ablation Run results file."""

    run_id: str
    golden_set: GoldenSetRef
    manifest: ManifestRef
    base_url: str
    harness_version: str
    run: AblationRun


def write_ablation_run(path: str | Path, run_file: AblationRunFile) -> None:
    """Write a run, its questions and its summary as versioned JSONL."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    run = run_file.run
    with path.open("w", encoding="utf-8") as out:
        out.write(_dump(_run_record(run_file)) + "\n")
        for question in run.questions:
            out.write(_dump(_question_record(question)) + "\n")
        out.write(_dump(_summary_record(run)) + "\n")


def _run_record(run_file: AblationRunFile) -> dict[str, Any]:
    """Build the header line of a results file."""
    run = run_file.run
    return {
        "schema_version": SCHEMA_VERSION,
        "type": "run",
        "run_id": run_file.run_id,
        "config": {
            "name": run.config.name,
            "techniques": list(run.config.techniques),
        },
        "top_k": run.top_k,
        "concurrency": run.concurrency,
        "golden_set": {
            "path": run_file.golden_set.path,
            "sha256": run_file.golden_set.sha256,
            "questions": run_file.golden_set.questions,
        },
        "manifest": {
            "path": run_file.manifest.path,
            "sha256": run_file.manifest.sha256,
        },
        "base_url": run_file.base_url,
        "harness_version": run_file.harness_version,
        "started_at": run.started_at,
    }


def _question_record(result: QuestionResult) -> dict[str, Any]:
    """Build one question line of a results file."""
    record: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "type": "question",
        "id": result.id,
        "slices": list(result.slices),
        "status": result.status.value,
        "evidence": list(result.evidence),
        "retrieved": [
            {
                "chunk_id": retrieved.chunk_id,
                "document_id": retrieved.document_id,
                "source_id": retrieved.source_id,
                "score": retrieved.score,
                "scores": retrieved.scores,
            }
            for retrieved in result.retrieved
        ],
        "trace": [_trace_record(step) for step in result.trace],
        "metrics": result.metrics.to_json() if result.metrics is not None else None,
        "latency_ms": result.latency_ms,
    }
    if result.error is not None:
        record["error"] = result.error
    return record


def _trace_record(step: TraceStep) -> dict[str, Any]:
    """Build one trace step of a question line."""
    record: dict[str, Any] = {
        "step": step.step,
        "technique": step.technique,
        "duration_ms": step.duration_ms,
    }
    if step.model is not None:
        record["model"] = step.model
    if step.candidates is not None:
        record["candidates"] = step.candidates
    return record


def _summary_record(run: AblationRun) -> dict[str, Any]:
    """Build the summary line of a results file."""
    return {
        "schema_version": SCHEMA_VERSION,
        "type": "summary",
        "metrics": run.summary.metrics,
        "counts": run.summary.counts,
        "latency_ms": {
            "p50": run.summary.latency_p50,
            "p95": run.summary.latency_p95,
        },
        "finished_at": run.finished_at,
    }


def _dump(value: Any) -> str:
    """Encode one JSONL line."""
    return json.dumps(value, ensure_ascii=False)


def load_ablation_run(path: str | Path) -> AblationRunFile:
    """Load and validate an Ablation Run results file."""
    path = Path(path)
    lines = path.read_text(encoding="utf-8").splitlines()
    if not lines:
        raise ValueError(f"{path}: empty Ablation Run file")

    records = [_decode_line(path, number, line) for number, line in enumerate(lines, 1)]
    first_where, first = records[0]
    if first.get("type") != "run":
        raise ValueError(f"{first_where}: first record must have type 'run'")
    if len(records) < 2:
        raise ValueError(f"{path}: expected a summary after the run header")
    last_where, last = records[-1]
    if last.get("type") != "summary":
        raise ValueError(
            f"{last_where}: last record must have type 'summary', got {last.get('type')!r}"
        )

    for where, value in records[1:-1]:
        if value.get("type") != "question":
            raise ValueError(
                f"{where}: only question records may follow the run header, "
                f"got {value.get('type')!r}"
            )

    header = _parse_run_record(first, first_where)
    questions = tuple(_parse_question_record(value, where) for where, value in records[1:-1])
    summary, finished_at = _parse_summary_record(last, last_where)
    return AblationRunFile(
        run_id=header.run_id,
        golden_set=header.golden_set,
        manifest=header.manifest,
        base_url=header.base_url,
        harness_version=header.harness_version,
        run=AblationRun(
            config=header.config,
            top_k=header.top_k,
            concurrency=header.concurrency,
            started_at=header.started_at,
            finished_at=finished_at,
            questions=questions,
            summary=summary,
        ),
    )


def _decode_line(path: Path, number: int, raw_line: str) -> tuple[str, dict[str, Any]]:
    """Decode one JSONL line and check its schema version."""
    where = f"{path}:{number}"
    if not raw_line.strip():
        raise ValueError(f"{where}: blank line (one JSON object per line)")
    try:
        value = json.loads(raw_line)
    except json.JSONDecodeError as exc:
        raise ValueError(f"{where}: invalid JSON: {exc.msg}") from exc
    if not isinstance(value, dict):
        raise ValueError(f"{where}: expected a JSON object")
    if value.get("schema_version") != SCHEMA_VERSION:
        raise ValueError(
            f"{where}: schema_version must be {SCHEMA_VERSION}, got {value.get('schema_version')!r}"
        )
    return where, value


def _parse_run_record(value: dict[str, Any], where: str) -> _RunHeader:
    """Validate the header line."""
    check_fields(
        value,
        {
            "schema_version",
            "type",
            "run_id",
            "config",
            "top_k",
            "concurrency",
            "golden_set",
            "manifest",
            "base_url",
            "harness_version",
            "started_at",
        },
        where,
    )
    return _RunHeader(
        run_id=string_field(value, "run_id", where),
        config=_parse_config(value.get("config"), f"{where}: config"),
        top_k=_top_k_field(value, where),
        golden_set=_parse_golden_set_ref(value.get("golden_set"), f"{where}: golden_set"),
        manifest=_parse_manifest_ref(value.get("manifest"), f"{where}: manifest"),
        base_url=string_field(value, "base_url", where),
        harness_version=string_field(value, "harness_version", where),
        started_at=string_field(value, "started_at", where),
        concurrency=_concurrency_field(value, where),
    )


def _concurrency_field(value: dict[str, Any], where: str) -> int:
    """Return a run header's concurrency, which must be one a run can use."""
    concurrency = int_field(value, "concurrency", where)
    if not 1 <= concurrency <= MAX_CONCURRENCY:
        raise ValueError(
            f"{where}: 'concurrency' must be between 1 and {MAX_CONCURRENCY}, got {concurrency}"
        )
    return concurrency


def _top_k_field(value: dict[str, Any], where: str) -> int:
    """Return a run header's top_k, which must be one the metrics were built for."""
    top_k = int_field(value, "top_k", where)
    if not 10 <= top_k <= 100:
        raise ValueError(f"{where}: 'top_k' must be between 10 and 100, got {top_k}")
    return top_k


def _parse_config(value: Any, where: str) -> RetrievalConfig:
    """Validate the config object of a run header."""
    if not isinstance(value, dict):
        raise ValueError(f"{where}: expected a JSON object")
    check_fields(value, {"name", "techniques"}, where)
    name = string_field(value, "name", where)
    techniques = string_list_field(value, "techniques", where)
    if not techniques:
        raise ValueError(f"{where}: 'techniques' must not be empty")
    if len(set(techniques)) != len(techniques):
        raise ValueError(f"{where}: 'techniques' must not list a technique twice")
    if tuple(sorted(techniques)) != techniques:
        raise ValueError(f"{where}: 'techniques' must be in canonical (sorted) order")
    expected = "+".join(techniques)
    if name != expected:
        raise ValueError(f"{where}: 'name' must be the join of the techniques, {expected!r}")
    return RetrievalConfig(name=name, techniques=techniques)


def _parse_golden_set_ref(value: Any, where: str) -> GoldenSetRef:
    """Validate the golden_set object of a run header."""
    if not isinstance(value, dict):
        raise ValueError(f"{where}: expected a JSON object")
    check_fields(value, {"path", "sha256", "questions"}, where)
    return GoldenSetRef(
        path=string_field(value, "path", where),
        sha256=string_field(value, "sha256", where),
        questions=int_field(value, "questions", where),
    )


def _parse_manifest_ref(value: Any, where: str) -> ManifestRef:
    """Validate the manifest object of a run header."""
    if not isinstance(value, dict):
        raise ValueError(f"{where}: expected a JSON object")
    check_fields(value, {"path", "sha256"}, where)
    return ManifestRef(
        path=string_field(value, "path", where),
        sha256=string_field(value, "sha256", where),
    )


def _parse_question_record(value: dict[str, Any], where: str) -> QuestionResult:
    """Validate one question line."""
    check_fields(
        value,
        {
            "schema_version",
            "type",
            "id",
            "slices",
            "status",
            "evidence",
            "retrieved",
            "trace",
            "metrics",
            "latency_ms",
            "error",
        },
        where,
        required={
            "schema_version",
            "type",
            "id",
            "slices",
            "status",
            "evidence",
            "retrieved",
            "trace",
            "metrics",
            "latency_ms",
        },
    )
    question_id = string_field(value, "id", where)
    slices = string_list_field(value, "slices", where)
    raw_status = value.get("status")
    try:
        status = QuestionStatus(raw_status)
    except (TypeError, ValueError):
        choices = ", ".join(repr(item.value) for item in QuestionStatus)
        raise ValueError(
            f"{where}: 'status' must be one of {choices}, got {raw_status!r}"
        ) from None
    evidence = string_list_field(value, "evidence", where)
    retrieved = _parse_retrieved(value.get("retrieved"), f"{where}: retrieved")
    trace = _parse_trace(value.get("trace"), f"{where}: trace")
    latency_ms = float_field(value, "latency_ms", where)
    error: str | None = None
    if "error" in value:
        error = string_field(value, "error", where)
    if status is QuestionStatus.ERROR and error is None:
        raise ValueError(f"{where}: an error record needs an 'error' message")
    if status is not QuestionStatus.ERROR and error is not None:
        raise ValueError(f"{where}: only an error record may carry 'error'")

    metrics_value = value.get("metrics")
    if status is QuestionStatus.SCORED:
        metrics = _parse_question_metrics(metrics_value, f"{where}: metrics")
    else:
        if metrics_value is not None:
            raise ValueError(
                f"{where}: metrics must be null unless status is {QuestionStatus.SCORED.value!r}"
            )
        metrics = None
    return QuestionResult(
        id=question_id,
        slices=slices,
        status=status,
        evidence=evidence,
        retrieved=retrieved,
        trace=trace,
        metrics=metrics,
        latency_ms=latency_ms,
        error=error,
    )


def _parse_retrieved(value: Any, where: str) -> tuple[RetrievedRecord, ...]:
    """Validate the retrieved list of a question line."""
    if not isinstance(value, list):
        raise ValueError(f"{where}: expected a list")
    records: list[RetrievedRecord] = []
    for index, item in enumerate(value):
        item_where = f"{where}[{index}]"
        if not isinstance(item, dict):
            raise ValueError(f"{item_where}: expected a JSON object")
        check_fields(item, {"chunk_id", "document_id", "source_id", "score", "scores"}, item_where)
        source_id = item.get("source_id")
        if source_id is not None and not isinstance(source_id, str):
            raise ValueError(f"{item_where}: 'source_id' must be a string or null")
        records.append(
            RetrievedRecord(
                chunk_id=string_field(item, "chunk_id", item_where),
                document_id=string_field(item, "document_id", item_where),
                source_id=source_id,
                score=float_field(item, "score", item_where),
                scores=float_map_field(item, "scores", item_where),
            )
        )
    return tuple(records)


def _parse_trace(value: Any, where: str) -> tuple[TraceStep, ...]:
    """Validate the trace list of a question line."""
    if not isinstance(value, list):
        raise ValueError(f"{where}: expected a list")
    steps: list[TraceStep] = []
    for index, item in enumerate(value):
        item_where = f"{where}[{index}]"
        if not isinstance(item, dict):
            raise ValueError(f"{item_where}: expected a JSON object")
        check_fields(
            item,
            {"step", "technique", "duration_ms", "model", "candidates"},
            item_where,
            required={"step", "technique", "duration_ms"},
        )
        steps.append(
            TraceStep(
                step=string_field(item, "step", item_where),
                technique=optional_string_field(item, "technique", item_where),
                duration_ms=int_field(item, "duration_ms", item_where),
                model=optional_string_field(item, "model", item_where),
                candidates=optional_int_field(item, "candidates", item_where),
            )
        )
    return tuple(steps)


def _parse_question_metrics(value: Any, where: str) -> QuestionMetrics:
    """Validate the metrics object of a scored question."""
    if not isinstance(value, dict):
        raise ValueError(f"{where}: expected a JSON object")
    check_fields(value, set(METRIC_NAMES), where)
    return QuestionMetrics(
        recall_at_5=float_field(value, RECALL_5, where),
        recall_at_10=float_field(value, RECALL_10, where),
        mrr=float_field(value, MRR, where),
        ndcg_at_10=float_field(value, NDCG_10, where),
    )


def _parse_summary_record(value: dict[str, Any], where: str) -> tuple[RunSummary, str]:
    """Validate the summary line and return it with its finished_at."""
    check_fields(
        value,
        {"schema_version", "type", "metrics", "counts", "latency_ms", "finished_at"},
        where,
    )
    metrics = value.get("metrics")
    if not isinstance(metrics, dict):
        raise ValueError(f"{where}: 'metrics' must be a JSON object")
    check_fields(metrics, set(METRIC_NAMES), f"{where}: metrics")
    counts = value.get("counts")
    if not isinstance(counts, dict):
        raise ValueError(f"{where}: 'counts' must be a JSON object")
    check_fields(counts, {status.value for status in QuestionStatus}, f"{where}: counts")
    latency = value.get("latency_ms")
    if not isinstance(latency, dict):
        raise ValueError(f"{where}: 'latency_ms' must be a JSON object")
    check_fields(latency, {"p50", "p95"}, f"{where}: latency_ms")
    finished_at = string_field(value, "finished_at", where)
    summary = RunSummary(
        recall_at_5=optional_float_field(metrics, RECALL_5, where),
        recall_at_10=optional_float_field(metrics, RECALL_10, where),
        mrr=optional_float_field(metrics, MRR, where),
        ndcg_at_10=optional_float_field(metrics, NDCG_10, where),
        scored=int_field(counts, "scored", where),
        skipped=int_field(counts, "skipped", where),
        error=int_field(counts, "error", where),
        latency_p50=optional_float_field(latency, "p50", where),
        latency_p95=optional_float_field(latency, "p95", where),
    )
    if min(summary.scored, summary.skipped, summary.error) < 0:
        raise ValueError(f"{where}: counts must not be negative")
    return summary, finished_at
