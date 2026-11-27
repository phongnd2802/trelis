"""Tests for ablation runs: manifest mapping, scoring, summaries and JSONL."""

import hashlib
import json
import math
from datetime import UTC, datetime

import httpx
import pytest

from trelis_eval.ablation_run import (
    HARNESS_VERSION,
    AblationAborted,
    AblationRunFile,
    GoldenSetRef,
    Manifest,
    ManifestRef,
    QuestionStatus,
    RetrievalConfig,
    default_run_id,
    file_sha256,
    load_ablation_run,
    load_manifest,
    parse_retrieval_config,
    run_ablation,
    write_ablation_run,
)
from trelis_eval.client import TraceStep, TrelisClient
from trelis_eval.golden import Evidence, GoldenQuestion, write_golden_set

BASE_URL = "http://trelis.test"
API_KEY = "test-key-never-logged"


def question(question_id, source_ids, slices=("inference_query", "hops-1"), principal=None):
    return GoldenQuestion(
        id=question_id,
        question=f"Question {question_id}?",
        expected_answer="An answer.",
        evidence=tuple(Evidence(source_id=source_id, fact="f") for source_id in source_ids),
        slices=slices,
        principal=principal,
    )


def write_manifest_lines(path, records):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(json.dumps(record) for record in records) + "\n", encoding="utf-8")
    return path


def chunk(document_id, chunk_id, score=0.5):
    return {
        "chunk_id": chunk_id,
        "document_id": document_id,
        "seq": 0,
        "content": "Chunk content that must never reach the results file.",
        "context_header": "# Header",
        "score": score,
        "scores": {"dense": score},
    }


TRACE = (
    {
        "step": "embed_query",
        "technique": "dense",
        "duration_ms": 4,
        "model": "text-embedding-3-small",
    },
    {"step": "search", "technique": "dense", "duration_ms": 6, "candidates": 2},
)


class RetrievalServer:
    """Answers POST /retrieve from a query-to-results map, or fails chosen queries."""

    def __init__(
        self,
        results_by_query,
        *,
        failing_queries=(),
        failure_status=400,
        trace=(),
    ):
        self.results_by_query = results_by_query
        self.failing_queries = set(failing_queries)
        self.failure_status = failure_status
        self.trace = list(trace)
        self.requests = []

    def __call__(self, request):
        body = json.loads(request.content)
        self.requests.append(body)
        if body["query"] in self.failing_queries:
            if self.failure_status == 400:
                code = 1010
                message = 'retrieval technique "bm25" is not implemented'
            else:
                code = 1000
                message = "boom"
            return httpx.Response(
                self.failure_status,
                json={
                    "success": False,
                    "error": {"code": code, "message": message, "details": None},
                },
            )
        return httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "results": self.results_by_query.get(body["query"], []),
                    "trace": self.trace,
                },
            },
        )


def make_client(handler):
    return TrelisClient(BASE_URL, API_KEY, transport=httpx.MockTransport(handler))


def scripted_clock(values):
    remaining = iter(values)

    def clock():
        value = next(remaining)
        return value

    return clock


def test_load_manifest_maps_completed_documents_only(tmp_path):
    path = write_manifest_lines(
        tmp_path / "manifest.jsonl",
        [
            {
                "source_id": "https://example.com/a",
                "document_id": "doc-a",
                "file_name": "a.md",
                "status": "completed",
                "duplicate": False,
                "error": "",
            },
            {
                "source_id": "https://example.com/b",
                "document_id": "doc-b",
                "file_name": "b.md",
                "status": "failed",
                "duplicate": False,
                "error": "parse: broken",
            },
        ],
    )

    manifest = load_manifest(path)

    assert manifest.source_by_document == {"doc-a": "https://example.com/a"}
    assert manifest.completed_source_ids == frozenset({"https://example.com/a"})


def test_load_manifest_rejects_a_completed_record_without_a_document_id(tmp_path):
    path = write_manifest_lines(
        tmp_path / "manifest.jsonl",
        [
            {
                "source_id": "https://example.com/a",
                "document_id": None,
                "file_name": "a.md",
                "status": "completed",
            }
        ],
    )

    with pytest.raises(ValueError, match="completed.*document_id"):
        load_manifest(path)


def test_load_manifest_names_a_malformed_line(tmp_path):
    path = tmp_path / "manifest.jsonl"
    path.write_text('{"source_id": "https://example.com/a"}\n', encoding="utf-8")

    with pytest.raises(ValueError, match=r"manifest.jsonl:1:"):
        load_manifest(path)


def test_run_scores_ranked_source_ids_and_never_counts_an_unmapped_chunk():
    golden = [
        question("q1", ["https://example.com/a", "https://example.com/b"]),
        question("q2", ["https://example.com/a"]),
    ]
    manifest = Manifest(
        source_by_document={
            "doc-a": "https://example.com/a",
            "doc-b": "https://example.com/b",
            "doc-x": "https://example.com/x",
        },
        completed_source_ids=frozenset(
            {"https://example.com/a", "https://example.com/b", "https://example.com/x"}
        ),
    )
    server = RetrievalServer(
        {
            "Question q1?": [
                chunk("doc-a", "c1"),
                chunk("doc-unmapped", "c2"),
                chunk("doc-b", "c3"),
            ],
            "Question q2?": [chunk("doc-b", "c4"), chunk("doc-a", "c5")],
        }
    )
    clock = scripted_clock([0.0, 0.01, 1.0, 1.02])

    with make_client(server) as client:
        run = run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=clock,
        )

    first, second = run.questions
    assert first.status == "scored"
    assert [record.source_id for record in first.retrieved] == [
        "https://example.com/a",
        None,
        "https://example.com/b",
    ]
    assert first.evidence == ("https://example.com/a", "https://example.com/b")
    assert first.metrics is not None
    assert first.metrics.recall_at_5 == 1.0
    assert first.metrics.recall_at_10 == 1.0
    assert first.metrics.mrr == 1.0
    expected_ndcg = (1 / math.log2(2) + 1 / math.log2(4)) / (1 / math.log2(2) + 1 / math.log2(3))
    assert first.metrics.ndcg_at_10 == pytest.approx(expected_ndcg)

    assert second.metrics is not None
    assert second.metrics.mrr == 0.5
    assert second.metrics.ndcg_at_10 == pytest.approx(1 / math.log2(3))
    assert server.requests[0]["top_k"] == 10
    assert server.requests[0]["retrieval_config"] == {"techniques": ["dense"]}

    assert run.summary.scored == 2
    assert run.summary.skipped == 0
    assert run.summary.error == 0
    assert run.summary.mrr == pytest.approx((1.0 + 0.5) / 2)
    assert run.summary.latency_p50 == 10.0
    assert run.summary.latency_p95 == 20.0


def test_run_fails_fast_when_evidence_has_no_completed_manifest_record():
    golden = [question("q1", ["https://example.com/missing"])]
    manifest = Manifest(source_by_document={}, completed_source_ids=frozenset())

    def handler(request):
        pytest.fail("no request may run before the evidence check")

    with make_client(handler) as client, pytest.raises(ValueError) as caught:
        run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=lambda: 0.0,
        )

    assert "https://example.com/missing" in str(caught.value)


def test_run_fails_fast_on_a_per_principal_question():
    golden = [question("mhr-0001", ["https://example.com/a"], principal="alice")]
    manifest = Manifest(
        source_by_document={"doc-a": "https://example.com/a"},
        completed_source_ids=frozenset({"https://example.com/a"}),
    )

    def handler(request):
        pytest.fail("no request may run before the Principal check")

    with make_client(handler) as client, pytest.raises(ValueError) as caught:
        run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=lambda: 0.0,
        )

    message = str(caught.value)
    assert "mhr-0001" in message
    assert "alice" in message
    assert "principal" in message.lower()


def test_run_skips_a_null_query_without_calling_retrieve():
    golden = [question("q-null", [], slices=("null_query", "hops-0"))]

    def handler(request):
        pytest.fail("a null query has no evidence to retrieve against")

    manifest = Manifest(source_by_document={}, completed_source_ids=frozenset())

    with make_client(handler) as client:
        run = run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=lambda: 0.0,
        )

    [result] = run.questions
    assert result.status == "skipped"
    assert result.evidence == ()
    assert result.retrieved == ()
    assert result.metrics is None
    assert result.latency_ms == 0.0
    assert run.summary.scored == 0
    assert run.summary.skipped == 1
    assert run.summary.metrics == {
        "recall@5": None,
        "recall@10": None,
        "mrr": None,
        "ndcg@10": None,
    }
    assert run.summary.latency_p50 is None
    assert run.summary.latency_p95 is None


def test_a_request_error_is_recorded_and_excluded_from_the_means():
    golden = [
        question("q1", ["https://example.com/a"]),
        question("q2", ["https://example.com/a"]),
    ]
    manifest = Manifest(
        source_by_document={"doc-a": "https://example.com/a"},
        completed_source_ids=frozenset({"https://example.com/a"}),
    )
    server = RetrievalServer(
        {"Question q1?": [chunk("doc-a", "c1")], "Question q2?": []},
        failing_queries={"Question q2?"},
        failure_status=500,
    )
    clock = scripted_clock([0.0, 0.01, 1.0, 1.03])

    with make_client(server) as client:
        run = run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=clock,
        )

    scored, errored = run.questions
    assert scored.status is QuestionStatus.SCORED
    assert errored.status is QuestionStatus.ERROR
    assert errored.metrics is None
    assert errored.retrieved == ()
    assert errored.trace == ()
    assert errored.latency_ms == 30.0
    assert "HTTP 500" in errored.error
    assert "1000" in errored.error
    assert run.summary.scored == 1
    assert run.summary.error == 1
    assert run.summary.skipped == 0
    assert run.summary.mrr == 1.0
    assert run.summary.latency_p50 == 10.0
    assert run.summary.latency_p95 == 10.0


def test_a_transport_error_is_recorded_and_excluded_from_the_means():
    golden = [
        question("q1", ["https://example.com/a"]),
        question("q2", ["https://example.com/a"]),
    ]
    manifest = Manifest(
        source_by_document={"doc-a": "https://example.com/a"},
        completed_source_ids=frozenset({"https://example.com/a"}),
    )

    def handler(request):
        body = json.loads(request.content)
        if body["query"] == "Question q2?":
            raise httpx.ConnectError("connection refused")
        return httpx.Response(
            200,
            json={
                "success": True,
                "data": {"results": [chunk("doc-a", "c1")], "trace": []},
            },
        )

    clock = scripted_clock([0.0, 0.01, 1.0, 1.03])

    with make_client(handler) as client:
        run = run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=clock,
        )

    scored, errored = run.questions
    assert scored.status is QuestionStatus.SCORED
    assert errored.status is QuestionStatus.ERROR
    assert "connection refused" in errored.error
    assert run.summary.scored == 1
    assert run.summary.error == 1
    assert run.summary.mrr == 1.0


@pytest.mark.parametrize("status", [400, 503])
def test_a_rejected_config_aborts_the_run_at_the_first_request(status):
    golden = [
        question("q1", ["https://example.com/a"]),
        question("q2", ["https://example.com/a"]),
    ]
    manifest = Manifest(
        source_by_document={"doc-a": "https://example.com/a"},
        completed_source_ids=frozenset({"https://example.com/a"}),
    )
    server = RetrievalServer(
        {},
        failing_queries={"Question q1?", "Question q2?"},
        failure_status=status,
    )

    with make_client(server) as client, pytest.raises(AblationAborted) as caught:
        run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="bm25", techniques=("bm25",)),
            clock=scripted_clock([0.0, 0.01, 1.0, 1.01]),
        )

    assert caught.value.status == status
    assert f"HTTP {status}" in str(caught.value)
    assert len(server.requests) == 1


def test_summary_means_and_nearest_rank_percentiles_use_the_scored_calls():
    golden = [
        question("q1", ["https://example.com/a"]),
        question("q2", ["https://example.com/a"]),
        question("q3", ["https://example.com/a"]),
    ]
    manifest = Manifest(
        source_by_document={
            "doc-a": "https://example.com/a",
            "doc-x": "https://example.com/x",
        },
        completed_source_ids=frozenset({"https://example.com/a", "https://example.com/x"}),
    )
    server = RetrievalServer(
        {
            "Question q1?": [chunk("doc-a", "c1")],
            "Question q2?": [chunk("doc-x", "c2")],
            "Question q3?": [chunk("doc-x", "c3"), chunk("doc-a", "c4")],
        }
    )
    clock = scripted_clock([0.0, 0.01, 1.0, 1.02, 2.0, 2.04])

    with make_client(server) as client:
        run = run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=clock,
        )

    assert run.summary.metrics == {
        "recall@5": pytest.approx(2 / 3),
        "recall@10": pytest.approx(2 / 3),
        "mrr": pytest.approx(0.5),
        "ndcg@10": pytest.approx((1.0 + 0.0 + 1 / math.log2(3)) / 3),
    }
    assert run.summary.latency_p50 == 20.0
    assert run.summary.latency_p95 == 40.0


def test_top_k_must_be_between_10_and_100():
    manifest = Manifest(source_by_document={}, completed_source_ids=frozenset())

    def handler(request):
        pytest.fail("an invalid top_k never reaches the API")

    for top_k in (0, 9, 101):
        with make_client(handler) as client, pytest.raises(ValueError, match="top_k"):
            run_ablation(
                client,
                [],
                manifest,
                RetrievalConfig(name="dense", techniques=("dense",)),
                top_k,
                clock=lambda: 0.0,
            )


@pytest.mark.parametrize(
    ("value", "name", "techniques"),
    [
        ("dense", "dense", ("dense",)),
        ("bm25+dense", "bm25+dense", ("bm25", "dense")),
        ("dense+bm25", "bm25+dense", ("bm25", "dense")),
        (" dense + bm25 ", "bm25+dense", ("bm25", "dense")),
    ],
)
def test_parse_retrieval_config_canonicalises_the_technique_order(value, name, techniques):
    config = parse_retrieval_config(value)

    assert config == RetrievalConfig(name=name, techniques=techniques)


@pytest.mark.parametrize("value", ["", " ", "+", "dense+", "+dense", "dense++bm25", "dense+dense"])
def test_parse_retrieval_config_rejects_bad_syntax(value):
    with pytest.raises(ValueError, match="retrieval config"):
        parse_retrieval_config(value)


def build_run_file(tmp_path, golden, server):
    manifest_path = write_manifest_lines(
        tmp_path / "manifest.jsonl",
        [
            {
                "source_id": "https://example.com/a",
                "document_id": "doc-a",
                "file_name": "a.md",
                "status": "completed",
                "duplicate": False,
                "error": "",
            }
        ],
    )
    manifest = load_manifest(manifest_path)
    golden_path = tmp_path / "golden.jsonl"
    write_golden_set(golden_path, golden)

    with make_client(server) as client:
        run = run_ablation(
            client,
            golden,
            manifest,
            RetrievalConfig(name="dense", techniques=("dense",)),
            clock=scripted_clock([0.0, 0.007]),
        )

    return AblationRunFile(
        run_id="20260101T000000Z",
        golden_set=GoldenSetRef(
            path=str(golden_path),
            sha256=file_sha256(golden_path),
            questions=len(golden),
        ),
        manifest=ManifestRef(path=str(manifest_path), sha256=file_sha256(manifest_path)),
        base_url=BASE_URL,
        harness_version=HARNESS_VERSION,
        run=run,
    )


def test_jsonl_round_trip_keeps_the_trace_hashes_and_never_the_api_key(tmp_path):
    golden = [question("q1", ["https://example.com/a"])]
    server = RetrievalServer(
        {"Question q1?": [chunk("doc-a", "chunk-1", score=0.9)]},
        trace=TRACE,
    )
    run_file = build_run_file(tmp_path, golden, server)
    path = tmp_path / "runs" / "20260101T000000Z" / "dense.jsonl"

    write_ablation_run(path, run_file)

    assert load_ablation_run(path) == run_file
    [result] = run_file.run.questions
    assert result.trace == (
        TraceStep(
            step="embed_query",
            technique="dense",
            duration_ms=4,
            model="text-embedding-3-small",
            candidates=None,
        ),
        TraceStep(step="search", technique="dense", duration_ms=6, model=None, candidates=2),
    )
    raw = path.read_text(encoding="utf-8")
    assert API_KEY not in raw
    assert "Chunk content" not in raw
    assert "# Header" not in raw
    lines = [json.loads(line) for line in raw.splitlines()]
    assert [line["type"] for line in lines] == ["run", "question", "summary"]
    assert {line["schema_version"] for line in lines} == {1}
    assert lines[0]["golden_set"] == {
        "path": str(tmp_path / "golden.jsonl"),
        "sha256": hashlib.sha256((tmp_path / "golden.jsonl").read_bytes()).hexdigest(),
        "questions": 1,
    }
    assert lines[0]["manifest"] == {
        "path": str(tmp_path / "manifest.jsonl"),
        "sha256": hashlib.sha256((tmp_path / "manifest.jsonl").read_bytes()).hexdigest(),
    }
    assert "api_key" not in lines[0]
    assert lines[1]["trace"] == list(TRACE)
    assert "content" not in lines[1]["retrieved"][0]
    assert "context_header" not in lines[1]["retrieved"][0]


def test_jsonl_reader_rejects_a_bad_schema_or_record_order(tmp_path):
    good_header = {
        "schema_version": 1,
        "type": "run",
        "run_id": "run",
        "config": {"name": "dense", "techniques": ["dense"]},
        "top_k": 10,
        "golden_set": {"path": "golden.jsonl", "sha256": "0" * 64, "questions": 0},
        "manifest": {"path": "manifest.jsonl", "sha256": "0" * 64},
        "base_url": BASE_URL,
        "harness_version": HARNESS_VERSION,
        "started_at": "2026-01-01T00:00:00+00:00",
    }
    good_summary = {
        "schema_version": 1,
        "type": "summary",
        "metrics": {"recall@5": None, "recall@10": None, "mrr": None, "ndcg@10": None},
        "counts": {"scored": 0, "skipped": 0, "error": 0},
        "latency_ms": {"p50": None, "p95": None},
        "finished_at": "2026-01-01T00:00:01+00:00",
    }

    def write(lines):
        path = tmp_path / "bad.jsonl"
        if lines:
            path.write_text(
                "\n".join(json.dumps(line) for line in lines) + "\n",
                encoding="utf-8",
            )
        else:
            path.write_text("", encoding="utf-8")
        return path

    cases = [
        ([{**good_header, "schema_version": 2}, good_summary], "schema_version"),
        ([{**good_header, "top_k": 9}, good_summary], "top_k"),
        ([good_header, {**good_summary, "type": "unknown"}], "type"),
        (
            [
                {
                    **good_header,
                    "config": {"name": "dense+bm25", "techniques": ["bm25", "dense"]},
                },
                good_summary,
            ],
            "name",
        ),
        ([good_summary], "first"),
        ([good_header, good_header, good_summary], "run"),
        ([good_header], "summary"),
        ([], "empty"),
    ]
    for lines, match in cases:
        with pytest.raises(ValueError, match=match):
            load_ablation_run(write(lines))


def test_jsonl_reader_requires_sorted_config_techniques(tmp_path):
    header = {
        "schema_version": 1,
        "type": "run",
        "run_id": "run",
        "config": {"name": "dense+bm25", "techniques": ["dense", "bm25"]},
        "top_k": 10,
        "golden_set": {"path": "golden.jsonl", "sha256": "0" * 64, "questions": 0},
        "manifest": {"path": "manifest.jsonl", "sha256": "0" * 64},
        "base_url": BASE_URL,
        "harness_version": HARNESS_VERSION,
        "started_at": "2026-01-01T00:00:00+00:00",
    }
    summary = {
        "schema_version": 1,
        "type": "summary",
        "metrics": {"recall@5": None, "recall@10": None, "mrr": None, "ndcg@10": None},
        "counts": {"scored": 0, "skipped": 0, "error": 0},
        "latency_ms": {"p50": None, "p95": None},
        "finished_at": "2026-01-01T00:00:01+00:00",
    }
    path = tmp_path / "bad.jsonl"
    path.write_text(json.dumps(header) + "\n" + json.dumps(summary) + "\n", encoding="utf-8")

    with pytest.raises(ValueError, match="canonical"):
        load_ablation_run(path)


def test_default_run_id_is_a_utc_timestamp():
    now = datetime(2026, 1, 2, 3, 4, 5, tzinfo=UTC)

    assert default_run_id(now=lambda: now) == "20260102T030405Z"
