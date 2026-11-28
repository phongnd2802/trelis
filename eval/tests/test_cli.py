"""Tests for the trelis-eval command line."""

import json
from pathlib import Path

import httpx
import pytest
from fakes import AnswerServer

from trelis_eval.cli import main
from trelis_eval.golden import (
    Evidence,
    GoldenQuestion,
    load_golden_set,
    write_golden_set,
)
from trelis_eval.model_proxy import (
    Fixture,
    FixtureError,
    FixtureStore,
    RecordedResponse,
)

FIXTURES = Path(__file__).parent / "fixtures"


def line(question_id, slices):
    return json.dumps(
        {
            "id": question_id,
            "question": "Who?",
            "expected_answer": "A",
            "evidence": [],
            "slices": slices,
        }
    )


def test_golden_validate_prints_the_count_and_slice_counts(tmp_path, capsys):
    path = tmp_path / "golden.jsonl"
    path.write_text(
        "\n".join(
            [
                line("q1", ["comparison_query", "hops-1"]),
                line("q2", ["comparison_query", "hops-2"]),
            ]
        )
        + "\n",
        encoding="utf-8",
    )

    assert main(["golden", "validate", str(path)]) == 0

    output = capsys.readouterr().out
    assert "2 questions" in output
    assert "comparison_query: 2" in output
    assert "hops-1: 1" in output


def test_golden_validate_prints_errors_and_exits_one(tmp_path, capsys):
    path = tmp_path / "golden.jsonl"
    path.write_text(
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": "A", "evidence": []}) + "\n",
        encoding="utf-8",
    )

    assert main(["golden", "validate", str(path)]) == 1

    error = capsys.readouterr().err
    assert f"{path}:1:" in error
    assert "missing field 'slices'" in error


class IngestServer:
    """A MockTransport handler that accepts uploads and completes each Document."""

    def __init__(self, final_status="completed"):
        self.final_status = final_status
        self.document_ids = []
        self.uploads = []

    def __call__(self, request):
        if request.method == "POST":
            document_id = f"doc-{len(self.document_ids) + 1}"
            self.document_ids.append(document_id)
            self.uploads.append(request.content)
            return self._document(document_id, "pending")
        document_id = request.url.path.rsplit("/", 1)[-1]
        return self._document(document_id, self.final_status)

    @staticmethod
    def _document(document_id, status):
        return httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "id": document_id,
                    "title": "ignored",
                    "file_name": "ignored.md",
                    "parse_status": status,
                    "parse_error": "parse: broken" if status == "failed" else "",
                },
            },
        )


class FlakyServer:
    """Fails the first upload and keeps one accepted Document's poll failing."""

    def __init__(self):
        self.posts = 0
        self.document_ids = []

    def __call__(self, request):
        if request.method == "POST":
            self.posts += 1
            if self.posts == 1:
                return self._error()
            document_id = f"doc-{len(self.document_ids) + 1}"
            self.document_ids.append(document_id)
            return IngestServer._document(document_id, "pending")
        document_id = request.url.path.rsplit("/", 1)[-1]
        if document_id == "doc-1":
            return self._error()
        return IngestServer._document(document_id, "completed")

    @staticmethod
    def _error():
        return httpx.Response(
            500,
            json={"success": False, "error": {"code": 1000, "message": "boom"}},
        )


def run_ingest(tmp_path, monkeypatch, transport, *extra):
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    monkeypatch.setenv("TRELIS_BASE_URL", "http://trelis.test")
    golden_out = tmp_path / "subset-golden.jsonl"
    manifest_out = tmp_path / "manifest.jsonl"
    code = main(
        [
            "multihop",
            "ingest",
            "--data-dir",
            str(FIXTURES),
            "--questions",
            "2",
            "--extra-docs",
            "1",
            "--seed",
            "7",
            "--principals",
            "alice,bob",
            "--golden-out",
            str(golden_out),
            "--manifest-out",
            str(manifest_out),
            "--poll-interval",
            "0",
            "--timeout",
            "5",
            *extra,
        ],
        transport=transport,
    )
    return code, golden_out, manifest_out


def test_multihop_ingest_writes_the_subset_golden_set_and_manifest(tmp_path, monkeypatch, capsys):
    server = IngestServer()

    code, golden_out, manifest_out = run_ingest(tmp_path, monkeypatch, httpx.MockTransport(server))

    assert code == 0
    golden = load_golden_set(golden_out)
    assert [question.id for question in golden] == ["mhr-0000", "mhr-0001"]
    manifest = [json.loads(line) for line in manifest_out.read_text(encoding="utf-8").splitlines()]
    assert [record["source_id"] for record in manifest] == [
        "https://example.com/alpha",
        "https://example.com/beta",
        "https://example.com/gamma",
        "https://example.com/delta",
    ]
    assert [record["document_id"] for record in manifest] == [
        "doc-1",
        "doc-2",
        "doc-3",
        "doc-4",
    ]
    assert {record["status"] for record in manifest} == {"completed"}
    assert all(b"alice,bob" in upload for upload in server.uploads)
    output = capsys.readouterr().out
    assert "completed=4" in output
    assert "failed=0" in output


def test_multihop_ingest_exits_one_when_a_document_fails(tmp_path, monkeypatch, capsys):
    server = IngestServer(final_status="failed")

    code, _, manifest_out = run_ingest(tmp_path, monkeypatch, httpx.MockTransport(server))

    assert code == 1
    manifest = [json.loads(line) for line in manifest_out.read_text(encoding="utf-8").splitlines()]
    assert {record["status"] for record in manifest} == {"failed"}
    assert {record["error"] for record in manifest} == {"parse: broken"}
    assert "failed=4" in capsys.readouterr().out


def test_multihop_ingest_writes_outputs_when_requests_fail(tmp_path, monkeypatch, capsys):
    server = FlakyServer()

    code, golden_out, manifest_out = run_ingest(
        tmp_path, monkeypatch, httpx.MockTransport(server), "--timeout", "0"
    )

    assert code == 1
    assert len(load_golden_set(golden_out)) == 2
    manifest = [json.loads(line) for line in manifest_out.read_text(encoding="utf-8").splitlines()]
    assert [record["status"] for record in manifest] == [
        "error",
        "timeout",
        "completed",
        "completed",
    ]
    assert manifest[0]["document_id"] is None
    assert "HTTP 500" in manifest[0]["error"]
    assert "HTTP 500" in manifest[1]["error"]
    output = capsys.readouterr().out
    assert "completed=2" in output
    assert "error=1" in output
    assert "timeout=1" in output


def test_multihop_ingest_requires_an_api_key(tmp_path, monkeypatch, capsys):
    monkeypatch.delenv("TRELIS_API_KEY", raising=False)

    def should_not_be_called(request):
        pytest.fail("the client was called without an API key")

    code = main(
        [
            "multihop",
            "ingest",
            "--data-dir",
            str(FIXTURES),
            "--questions",
            "1",
        ],
        transport=httpx.MockTransport(should_not_be_called),
    )

    assert code != 0
    assert "TRELIS_API_KEY is not set" in capsys.readouterr().err


def test_multihop_golden_writes_the_full_golden_set(tmp_path, capsys):
    out = tmp_path / "golden.jsonl"

    assert main(["multihop", "golden", "--data-dir", str(FIXTURES), "--out", str(out)]) == 0

    assert len(load_golden_set(out)) == 4
    assert "4 Golden questions" in capsys.readouterr().out


def test_multihop_ingest_stratified_uses_the_sample_and_its_evidence(tmp_path, monkeypatch, capsys):
    server = IngestServer()

    code, golden_out, manifest_out = run_ingest(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(server),
        "--sample",
        "stratified",
    )

    assert code == 0
    golden = load_golden_set(golden_out)
    assert [question.id for question in golden] == ["mhr-0000", "mhr-0001"]
    manifest = [json.loads(line) for line in manifest_out.read_text(encoding="utf-8").splitlines()]
    assert "https://example.com/alpha" in {record["source_id"] for record in manifest}
    assert "https://example.com/beta" in {record["source_id"] for record in manifest}
    assert "4" in capsys.readouterr().out


def retrieval_question(question_id, source_ids):
    return GoldenQuestion(
        id=question_id,
        question=f"Question {question_id}?",
        expected_answer="An answer.",
        evidence=tuple(Evidence(source_id=source_id, fact="f") for source_id in source_ids),
        slices=("inference_query", "hops-1"),
    )


def retrieval_chunk(document_id, chunk_id):
    return {
        "chunk_id": chunk_id,
        "document_id": document_id,
        "seq": 0,
        "content": "The retrieved content.",
        "context_header": "# Header",
        "score": 0.9,
        "scores": {"dense": 0.9},
    }


class RetrievalServer:
    """A MockTransport handler for POST /retrieve."""

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
            code = 1010 if self.failure_status == 400 else 1000
            return httpx.Response(
                self.failure_status,
                json={
                    "success": False,
                    "error": {"code": code, "message": "request failed", "details": None},
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


def write_retrieval_inputs(tmp_path, questions, records):
    golden_path = tmp_path / "golden.jsonl"
    write_golden_set(golden_path, questions)
    manifest_path = tmp_path / "manifest.jsonl"
    manifest_path.write_text(
        ("\n".join(json.dumps(record) for record in records) + "\n") if records else "",
        encoding="utf-8",
    )
    return golden_path, manifest_path


def completed_record(source_id, document_id):
    return {
        "source_id": source_id,
        "document_id": document_id,
        "file_name": "a.md",
        "status": "completed",
        "duplicate": False,
        "error": "",
    }


def run_retrieval(tmp_path, monkeypatch, transport, *extra):
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    monkeypatch.setenv("TRELIS_BASE_URL", "http://trelis.test")
    questions = [
        retrieval_question("mhr-0000", ["https://example.com/a"]),
        retrieval_question("mhr-0001", ["https://example.com/a"]),
    ]
    golden_path, manifest_path = write_retrieval_inputs(
        tmp_path,
        questions,
        [completed_record("https://example.com/a", "doc-a")],
    )
    out_dir = tmp_path / "runs"
    code = main(
        [
            "retrieval",
            "run",
            "--golden",
            str(golden_path),
            "--manifest",
            str(manifest_path),
            "--out-dir",
            str(out_dir),
            "--run-id",
            "20260101T000000Z",
            *extra,
        ],
        transport=transport,
    )
    return code, out_dir


def test_retrieval_run_writes_one_file_per_config_under_one_run_id(tmp_path, monkeypatch, capsys):
    server = RetrievalServer(
        {
            "Question mhr-0000?": [retrieval_chunk("doc-a", "chunk-1")],
            "Question mhr-0001?": [retrieval_chunk("doc-a", "chunk-2")],
        }
    )

    code, out_dir = run_retrieval(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(server),
        "--config",
        "dense",
        "--config",
        "bm25+dense",
    )

    assert code == 0
    run_dir = out_dir / "20260101T000000Z"
    assert sorted(path.name for path in run_dir.iterdir()) == [
        "bm25+dense.jsonl",
        "dense.jsonl",
    ]
    lines = [
        json.loads(line)
        for line in (run_dir / "dense.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    assert lines[0]["run_id"] == "20260101T000000Z"
    assert lines[0]["config"] == {"name": "dense", "techniques": ["dense"]}
    assert lines[0]["golden_set"]["questions"] == 2
    assert lines[-1]["counts"] == {"scored": 2, "skipped": 0, "error": 0}
    assert "test-key" not in (run_dir / "dense.jsonl").read_text(encoding="utf-8")
    output = capsys.readouterr().out
    assert "dense: scored=2 skipped=0 error=0" in output
    assert "bm25+dense: scored=2 skipped=0 error=0" in output
    assert str(run_dir / "dense.jsonl") in output
    assert str(run_dir / "bm25+dense.jsonl") in output


def test_retrieval_run_defaults_to_one_dense_config(tmp_path, monkeypatch):
    server = RetrievalServer(
        {
            "Question mhr-0000?": [retrieval_chunk("doc-a", "chunk-1")],
            "Question mhr-0001?": [retrieval_chunk("doc-a", "chunk-2")],
        }
    )

    code, out_dir = run_retrieval(tmp_path, monkeypatch, httpx.MockTransport(server))

    assert code == 0
    assert [path.name for path in (out_dir / "20260101T000000Z").iterdir()] == ["dense.jsonl"]


def test_retrieval_run_rejects_bad_config_syntax_before_calling_trelis(
    tmp_path, monkeypatch, capsys
):
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    golden_path, manifest_path = write_retrieval_inputs(
        tmp_path,
        [retrieval_question("mhr-0000", ["https://example.com/a"])],
        [completed_record("https://example.com/a", "doc-a")],
    )

    def should_not_be_called(request):
        pytest.fail("a malformed --config must fail before any request")

    code = main(
        [
            "retrieval",
            "run",
            "--golden",
            str(golden_path),
            "--manifest",
            str(manifest_path),
            "--out-dir",
            str(tmp_path / "runs"),
            "--config",
            "dense+",
        ],
        transport=httpx.MockTransport(should_not_be_called),
    )

    assert code == 1
    assert "invalid retrieval config" in capsys.readouterr().err


def test_retrieval_run_rejects_two_configs_with_the_same_canonical_name(
    tmp_path, monkeypatch, capsys
):
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    golden_path, manifest_path = write_retrieval_inputs(
        tmp_path,
        [retrieval_question("mhr-0000", ["https://example.com/a"])],
        [completed_record("https://example.com/a", "doc-a")],
    )

    def should_not_be_called(request):
        pytest.fail("duplicate configs must fail before any request")

    code = main(
        [
            "retrieval",
            "run",
            "--golden",
            str(golden_path),
            "--manifest",
            str(manifest_path),
            "--out-dir",
            str(tmp_path / "runs"),
            "--config",
            "dense+bm25",
            "--config",
            "bm25+dense",
        ],
        transport=httpx.MockTransport(should_not_be_called),
    )

    assert code == 1
    assert "more than once" in capsys.readouterr().err


def test_retrieval_run_exits_one_when_no_question_was_scored(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    golden_path, manifest_path = write_retrieval_inputs(
        tmp_path,
        [
            GoldenQuestion(
                id="mhr-0000",
                question="Unanswerable?",
                expected_answer="Insufficient information.",
                evidence=(),
                slices=("null_query", "hops-0"),
            )
        ],
        [],
    )
    out_dir = tmp_path / "runs"

    def should_not_be_called(request):
        pytest.fail("a null query needs no request")

    code = main(
        [
            "retrieval",
            "run",
            "--golden",
            str(golden_path),
            "--manifest",
            str(manifest_path),
            "--out-dir",
            str(out_dir),
            "--run-id",
            "run-1",
        ],
        transport=httpx.MockTransport(should_not_be_called),
    )

    assert code == 1
    lines = [
        json.loads(line)
        for line in (out_dir / "run-1" / "dense.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    assert lines[1]["status"] == "skipped"
    assert lines[-1]["counts"] == {"scored": 0, "skipped": 1, "error": 0}
    assert "scored=0 skipped=1 error=0" in capsys.readouterr().out


def test_retrieval_run_exits_zero_and_prints_the_first_question_error(
    tmp_path, monkeypatch, capsys
):
    server = RetrievalServer(
        {"Question mhr-0000?": [retrieval_chunk("doc-a", "chunk-1")]},
        failing_queries={"Question mhr-0001?"},
        failure_status=500,
    )

    code, out_dir = run_retrieval(tmp_path, monkeypatch, httpx.MockTransport(server))

    assert code == 0
    lines = [
        json.loads(line)
        for line in (out_dir / "20260101T000000Z" / "dense.jsonl")
        .read_text(encoding="utf-8")
        .splitlines()
    ]
    assert lines[2]["status"] == "error"
    assert "HTTP 500" in lines[2]["error"]
    assert lines[-1]["counts"] == {"scored": 1, "skipped": 0, "error": 1}
    output = capsys.readouterr().out
    assert "error=1" in output
    assert "first_error=" in output
    assert "HTTP 500" in output


@pytest.mark.parametrize("top_k", ["0", "9", "101"])
def test_retrieval_run_requires_top_k_between_10_and_100(tmp_path, monkeypatch, capsys, top_k):
    def should_not_be_called(request):
        pytest.fail("an invalid top_k must fail before any request")

    code, _ = run_retrieval(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(should_not_be_called),
        "--top-k",
        top_k,
    )

    assert code == 1
    assert "between 10 and 100" in capsys.readouterr().err


def test_retrieval_run_refuses_to_overwrite_an_existing_results_file(tmp_path, monkeypatch, capsys):
    existing = tmp_path / "runs" / "20260101T000000Z" / "dense.jsonl"
    existing.parent.mkdir(parents=True)
    existing.write_text("earlier run\n", encoding="utf-8")

    def should_not_be_called(request):
        pytest.fail("an existing results file must fail before any request")

    code, _ = run_retrieval(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(should_not_be_called),
    )

    assert code == 1
    assert str(existing) in capsys.readouterr().err
    assert existing.read_text(encoding="utf-8") == "earlier run\n"


def test_retrieval_run_aborts_one_config_and_continues_with_the_next(tmp_path, monkeypatch, capsys):
    requests = []

    def handler(request):
        body = json.loads(request.content)
        requests.append(body)
        if "bm25" in body["retrieval_config"]["techniques"]:
            return httpx.Response(
                400,
                json={
                    "success": False,
                    "error": {
                        "code": 1010,
                        "message": 'retrieval technique "bm25" is not implemented',
                        "details": None,
                    },
                },
            )
        return httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "results": [retrieval_chunk("doc-a", f"chunk-{len(requests)}")],
                    "trace": [],
                },
            },
        )

    code, out_dir = run_retrieval(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(handler),
        "--config",
        "bm25",
        "--config",
        "dense",
    )

    assert code == 1
    run_dir = out_dir / "20260101T000000Z"
    assert [path.name for path in run_dir.iterdir()] == ["dense.jsonl"]
    output = capsys.readouterr().out
    assert "config bm25: aborted:" in output
    assert "dense: scored=2 skipped=0 error=0" in output
    assert len(requests) == 3


def test_retrieval_run_aborts_a_config_after_repeated_question_errors(
    tmp_path, monkeypatch, capsys
):
    requests = []

    def handler(request):
        body = json.loads(request.content)
        requests.append(body)
        return httpx.Response(
            502,
            json={
                "success": False,
                "error": {
                    "code": 1012,
                    "message": "the rerank model provider rejected the call (HTTP 402)",
                    "details": None,
                },
            },
        )

    code, out_dir = run_retrieval(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(handler),
        "--max-consecutive-errors",
        "2",
    )

    assert code == 1
    assert not (out_dir / "20260101T000000Z" / "dense.jsonl").exists()
    output = capsys.readouterr().out
    assert "config dense: aborted: 2 consecutive question errors; last: HTTP 502" in output
    assert len(requests) == 2


def test_retrieval_run_zero_max_consecutive_errors_keeps_the_question_errors(
    tmp_path, monkeypatch, capsys
):
    def handler(request):
        return httpx.Response(
            502,
            json={
                "success": False,
                "error": {
                    "code": 1012,
                    "message": "the rerank model provider rejected the call (HTTP 402)",
                    "details": None,
                },
            },
        )

    code, out_dir = run_retrieval(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(handler),
        "--max-consecutive-errors",
        "0",
    )

    assert code == 1
    path = out_dir / "20260101T000000Z" / "dense.jsonl"
    assert path.exists()
    summary = json.loads(path.read_text(encoding="utf-8").splitlines()[-1])
    assert summary["counts"]["error"] == 2
    output = capsys.readouterr().out
    assert "aborted" not in output


def test_retrieval_run_passes_request_timeout_and_concurrency(tmp_path, monkeypatch):
    import trelis_eval.cli as cli

    seen = {}
    real_client = cli.TrelisClient

    def spy_client(*args, **kwargs):
        seen["timeout"] = kwargs.get("timeout")
        return real_client(*args, **kwargs)

    monkeypatch.setattr(cli, "TrelisClient", spy_client)
    server = RetrievalServer({"Question mhr-0000?": [retrieval_chunk("doc-a", "chunk-1")]})

    code, out_dir = run_retrieval(
        tmp_path,
        monkeypatch,
        httpx.MockTransport(server),
        "--request-timeout",
        "90",
        "--concurrency",
        "3",
    )

    assert code == 0
    assert seen["timeout"] == 90.0
    header = json.loads(
        (out_dir / "20260101T000000Z" / "dense.jsonl").read_text(encoding="utf-8").splitlines()[0]
    )
    assert header["concurrency"] == 3


def test_retrieval_run_defaults_to_a_120s_request_timeout_and_one_request_at_a_time(
    tmp_path, monkeypatch
):
    import trelis_eval.cli as cli

    seen = {}
    real_client = cli.TrelisClient

    def spy_client(*args, **kwargs):
        seen["timeout"] = kwargs.get("timeout")
        return real_client(*args, **kwargs)

    monkeypatch.setattr(cli, "TrelisClient", spy_client)
    code, out_dir = run_retrieval(tmp_path, monkeypatch, httpx.MockTransport(RetrievalServer({})))

    assert code == 0
    assert seen["timeout"] == 120.0
    header = json.loads(
        (out_dir / "20260101T000000Z" / "dense.jsonl").read_text(encoding="utf-8").splitlines()[0]
    )
    assert header["concurrency"] == 1


def answer_arguments(tmp_path, *extra):
    """Write the two-answer Golden Set inputs and return the answer-run argv."""
    questions = [
        retrieval_question("mhr-0000", ["https://example.com/a"]),
        retrieval_question("mhr-0001", ["https://example.com/a"]),
    ]
    golden_path, manifest_path = write_retrieval_inputs(
        tmp_path,
        questions,
        [completed_record("https://example.com/a", "doc-a")],
    )
    return [
        "answer",
        "run",
        "--golden",
        str(golden_path),
        "--manifest",
        str(manifest_path),
        "--out-dir",
        str(tmp_path / "runs"),
        "--run-id",
        "20260101T000000Z",
        *extra,
    ]


def set_judge_env(monkeypatch):
    monkeypatch.setenv("TRELIS_EVAL_JUDGE_API_KEY", "judge-key-never-logged")
    monkeypatch.setenv("TRELIS_EVAL_JUDGE_BASE_URL", "http://judge.test/v1")
    monkeypatch.setenv("TRELIS_EVAL_JUDGE_MODEL", "judge-model")


def test_answer_run_without_judge_config_fails_before_any_request(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    for name in (
        "TRELIS_EVAL_JUDGE_API_KEY",
        "TRELIS_EVAL_JUDGE_BASE_URL",
        "TRELIS_EVAL_JUDGE_MODEL",
    ):
        monkeypatch.delenv(name, raising=False)

    def should_not_be_called(request):
        pytest.fail("missing judge config must fail before any request")

    code = main(answer_arguments(tmp_path), transport=httpx.MockTransport(should_not_be_called))

    assert code == 1
    error = capsys.readouterr().err
    assert error.startswith("error: ")
    assert "TRELIS_EVAL_JUDGE" in error
    assert len(error.splitlines()) == 1
    assert not (tmp_path / "runs" / "20260101T000000Z" / "dense.jsonl").exists()


def test_answer_run_rejects_an_unknown_grader_before_any_request(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    set_judge_env(monkeypatch)

    def should_not_be_called(request):
        pytest.fail("an unknown Grader must fail before any request")

    code = main(
        answer_arguments(tmp_path, "--graders", "nope"),
        transport=httpx.MockTransport(should_not_be_called),
    )

    assert code == 1
    error = capsys.readouterr().err
    assert "nope" in error
    assert "correctness" in error


def test_answer_run_writes_grades_the_judge_model_and_the_summary(tmp_path, monkeypatch, capsys):
    import trelis_eval.cli as cli

    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    monkeypatch.setenv("TRELIS_BASE_URL", "http://trelis.test")
    # The flags below must win over these environment values.
    monkeypatch.setenv("TRELIS_EVAL_JUDGE_API_KEY", "judge-key-never-logged")
    monkeypatch.setenv("TRELIS_EVAL_JUDGE_BASE_URL", "http://env-judge.test/v1")
    monkeypatch.setenv("TRELIS_EVAL_JUDGE_MODEL", "env-judge-model")
    server = AnswerServer(
        {
            "Question mhr-0000?": {
                "answer": "An answer.",
                "results": [retrieval_chunk("doc-a", "chunk-1")],
            },
            "Question mhr-0001?": {
                "answer": "A different answer.",
                "results": [retrieval_chunk("doc-a", "chunk-2")],
            },
        }
    )
    seen = {}
    real_judge = cli.JudgeClient

    def judge_handler(_request):
        return httpx.Response(
            200,
            json={
                "choices": [
                    {
                        "message": {
                            "role": "assistant",
                            "content": '{"correct": true, "rationale": "same claim"}',
                        }
                    }
                ]
            },
        )

    def spy_judge(base_url, api_key, model, **kwargs):
        seen["base_url"] = base_url
        seen["api_key"] = api_key
        seen["model"] = model
        return real_judge(
            base_url,
            api_key,
            model,
            transport=httpx.MockTransport(judge_handler),
            **kwargs,
        )

    monkeypatch.setattr(cli, "JudgeClient", spy_judge)

    code = main(
        answer_arguments(
            tmp_path,
            "--judge-base-url",
            "http://flag-judge.test/v1",
            "--judge-model",
            "flag-judge-model",
        ),
        transport=httpx.MockTransport(server),
    )

    assert code == 0
    assert seen["base_url"] == "http://flag-judge.test/v1"
    assert seen["model"] == "flag-judge-model"
    assert seen["api_key"] == "judge-key-never-logged"
    lines = [
        json.loads(line)
        for line in (tmp_path / "runs" / "20260101T000000Z" / "dense.jsonl")
        .read_text(encoding="utf-8")
        .splitlines()
    ]
    assert lines[0]["kind"] == "answer"
    assert lines[0]["judge_model"] == "flag-judge-model"
    assert lines[0]["graders"] == ["correctness"]
    assert lines[1]["grades"]["correctness"]["score"] == 1.0
    assert lines[-1]["grades"]["correctness"] == {
        "mean": 1.0,
        "ok": 2,
        "grader_error": 0,
    }
    output = capsys.readouterr().out
    assert "dense: scored=2 skipped=0 error=0" in output
    assert "correctness" in output
    assert "grader_error=0" in output
    assert "judge-key-never-logged" not in output
    raw = (tmp_path / "runs" / "20260101T000000Z" / "dense.jsonl").read_text(encoding="utf-8")
    assert "judge-key-never-logged" not in raw


def test_proxy_record_requires_an_upstream(capsys):
    code = main(["proxy", "--mode", "record", "--fixtures", "unused"])

    assert code == 1
    assert "at least one --upstream" in capsys.readouterr().err


def test_proxy_rejects_a_malformed_upstream_before_listening(capsys):
    code = main(
        ["proxy", "--mode", "record", "--upstream", "Embedding=https://openrouter.ai/api/v1"]
    )

    assert code == 1
    assert "--upstream" in capsys.readouterr().err


def test_proxy_rejects_a_malformed_listen_address_before_listening(capsys):
    code = main(["proxy", "--mode", "replay", "--listen", "not-an-address"])

    assert code == 1
    assert "--listen" in capsys.readouterr().err


def test_proxy_without_mode_fails_with_one_error_line(capsys):
    code = main(["proxy"])

    assert code == 1
    error = capsys.readouterr().err
    assert error.startswith("error: ")
    assert "--mode" in error
    assert len(error.splitlines()) == 1


def save_fixture(fixtures: Path, role: str, request: dict, key: str) -> None:
    """Save one fixture in the layout the proxy writes."""
    FixtureStore(fixtures).save(
        Fixture(
            key=key,
            role=role,
            path="/embeddings",
            request=request,
            response=RecordedResponse(
                status=200,
                content_type="application/json",
                stream=False,
                chunks=(b"{}",),
            ),
            recorded_at="2026-01-01T00:00:00Z",
        )
    )


def test_proxy_fixtures_counts_each_role_and_model(tmp_path, capsys):
    fixtures = tmp_path / "fixtures"
    save_fixture(fixtures, "embedding", {"model": "model-a", "input": ["a"]}, "a" * 64)
    save_fixture(fixtures, "embedding", {"model": "model-a", "input": ["b"]}, "b" * 64)
    save_fixture(fixtures, "embedding", {"model": "model-b", "input": ["c"]}, "c" * 64)
    save_fixture(fixtures, "rerank", {"model": "model-c", "query": "q"}, "d" * 64)
    (fixtures / "misses.jsonl").write_text("{}\n", encoding="utf-8")

    assert main(["proxy", "fixtures", "--fixtures", str(fixtures)]) == 0

    assert capsys.readouterr().out.splitlines() == [
        "embedding model-a: 2 fixtures",
        "embedding model-b: 1 fixture",
        "rerank model-c: 1 fixture",
    ]


def test_proxy_fixtures_lists_a_fixture_without_a_model_last(tmp_path, capsys):
    fixtures = tmp_path / "fixtures"
    save_fixture(fixtures, "embedding", {"model": "model-a", "input": ["a"]}, "a" * 64)
    save_fixture(fixtures, "embedding", {"input": ["b"]}, "b" * 64)

    assert main(["proxy", "fixtures", "--fixtures", str(fixtures)]) == 0

    assert capsys.readouterr().out.splitlines() == [
        "embedding model-a: 1 fixture",
        "embedding (no model): 1 fixture",
    ]


def test_proxy_fixtures_says_so_for_an_empty_tree(tmp_path, capsys):
    fixtures = tmp_path / "fixtures"

    assert main(["proxy", "fixtures", "--fixtures", str(fixtures)]) == 0

    assert capsys.readouterr().out.strip() == f"No fixtures in {fixtures}"


def test_proxy_fixtures_reports_a_broken_tree_as_one_error_line(tmp_path, capsys):
    fixtures = tmp_path / "fixtures"
    shard = fixtures / "embedding" / "ab"
    shard.mkdir(parents=True)
    (shard / ("ab" + "0" * 62 + ".json")).write_text("{", encoding="utf-8")

    try:
        code = main(["proxy", "fixtures", "--fixtures", str(fixtures)])
    except FixtureError:
        pytest.fail("the CLI let FixtureError escape instead of printing one error line")

    assert code == 1
    error = capsys.readouterr().err
    assert error.startswith("error: ")
    assert "is not valid JSON" in error
    assert len(error.splitlines()) == 1


def test_proxy_fixtures_rejects_a_mode(tmp_path, capsys):
    code = main(["proxy", "--mode", "replay", "fixtures", "--fixtures", str(tmp_path)])

    assert code == 1
    error = capsys.readouterr().err
    assert "--mode" in error
    assert "fixtures" in error


@pytest.mark.parametrize(
    ("flag", "value"),
    [("--concurrency", "0"), ("--concurrency", "33"), ("--request-timeout", "0")],
)
def test_retrieval_run_rejects_bad_timeout_or_concurrency(
    tmp_path, monkeypatch, capsys, flag, value
):
    def should_not_be_called(request):
        pytest.fail("a bad flag must fail before any request")

    with pytest.raises(SystemExit) as caught:
        run_retrieval(tmp_path, monkeypatch, httpx.MockTransport(should_not_be_called), flag, value)

    assert caught.value.code == 2


def test_retrieval_run_prints_per_step_latencies(tmp_path, monkeypatch, capsys):
    trace = [
        {"step": "embed_query", "technique": "dense", "duration_ms": 4, "model": "m"},
        {"step": "search", "technique": "dense", "duration_ms": 19000, "candidates": 1},
    ]
    server = RetrievalServer({}, trace=trace)

    code, _ = run_retrieval(tmp_path, monkeypatch, httpx.MockTransport(server))

    assert code == 0
    output = capsys.readouterr().out
    assert (
        "steps: dense/embed_query p50=4ms p95=4ms, dense/search p50=19000ms p95=19000ms" in output
    )


def write_varied_results_file(tmp_path, monkeypatch):
    """Write one results file whose questions rank the evidence differently."""
    monkeypatch.setenv("TRELIS_API_KEY", "test-key")
    monkeypatch.setenv("TRELIS_BASE_URL", "http://trelis.test")
    questions = [
        retrieval_question(f"mhr-{index:04d}", ["https://example.com/a"]) for index in range(8)
    ]
    golden_path, manifest_path = write_retrieval_inputs(
        tmp_path,
        questions,
        [completed_record("https://example.com/a", "doc-a")],
    )
    # Evidence at ranks 1 to 8 behind unmapped Chunks, so MRR and nDCG@10 vary
    # between questions and their intervals depend on the seed.
    server = RetrievalServer(
        {
            f"Question mhr-{index:04d}?": [
                *(retrieval_chunk(f"doc-x{rank}", f"c{index}-{rank}") for rank in range(index)),
                retrieval_chunk("doc-a", f"c{index}-a"),
            ]
            for index in range(8)
        }
    )
    out_dir = tmp_path / "runs"
    code = main(
        [
            "retrieval",
            "run",
            "--golden",
            str(golden_path),
            "--manifest",
            str(manifest_path),
            "--out-dir",
            str(out_dir),
            "--run-id",
            "20260101T000000Z",
        ],
        transport=httpx.MockTransport(server),
    )
    assert code == 0
    return out_dir / "20260101T000000Z" / "dense.jsonl"


def test_report_writes_byte_identical_output_on_a_rebuild(tmp_path, monkeypatch, capsys):
    results = write_varied_results_file(tmp_path, monkeypatch)
    monkeypatch.delenv("TRELIS_API_KEY")
    first = tmp_path / "report-1"
    second = tmp_path / "report-2"

    assert main(["report", str(results), "--resamples", "100", "--out-dir", str(first)]) == 0
    assert main(["report", str(results), "--resamples", "100", "--out-dir", str(second)]) == 0

    for name in ("ablation-matrix.md", "ablation-matrix.html"):
        assert (first / name).read_bytes() == (second / name).read_bytes()
    output = capsys.readouterr().out
    assert str(first / "ablation-matrix.md") in output
    assert str(first / "ablation-matrix.html") in output


def test_report_accepts_a_directory_of_results_files(tmp_path, monkeypatch):
    results = write_varied_results_file(tmp_path, monkeypatch)
    out_dir = tmp_path / "report"

    code = main(["report", str(results.parent), "--resamples", "50", "--out-dir", str(out_dir)])

    assert code == 0
    markdown = (out_dir / "ablation-matrix.md").read_text(encoding="utf-8")
    assert "| dense |" in markdown
    assert (out_dir / "ablation-matrix.html").exists()


def test_report_rejects_a_directory_without_results_files(tmp_path, capsys):
    empty = tmp_path / "empty"
    empty.mkdir()

    code = main(["report", str(empty), "--out-dir", str(tmp_path / "report")])

    assert code == 1
    assert "no *.jsonl" in capsys.readouterr().err


def test_report_a_different_seed_moves_the_intervals(tmp_path, monkeypatch):
    results = write_varied_results_file(tmp_path, monkeypatch)
    first = tmp_path / "seed-1"
    second = tmp_path / "seed-2"

    assert (
        main(["report", str(results), "--seed", "1", "--resamples", "500", "--out-dir", str(first)])
        == 0
    )
    assert (
        main(
            ["report", str(results), "--seed", "2", "--resamples", "500", "--out-dir", str(second)]
        )
        == 0
    )

    def matrix_rows(out_dir):
        lines = (out_dir / "ablation-matrix.md").read_text(encoding="utf-8").splitlines()
        return [line for line in lines if line.startswith("| dense |")]

    assert matrix_rows(first) != matrix_rows(second)


def test_report_exits_one_on_results_from_different_golden_sets(tmp_path, monkeypatch, capsys):
    results = write_varied_results_file(tmp_path, monkeypatch)
    lines = results.read_text(encoding="utf-8").splitlines()
    header = json.loads(lines[0])
    header["config"] = {"name": "bm25", "techniques": ["bm25"]}
    header["golden_set"]["sha256"] = "0" * 64
    other = tmp_path / "other" / "bm25.jsonl"
    other.parent.mkdir(parents=True)
    other.write_text("\n".join([json.dumps(header), *lines[1:]]) + "\n", encoding="utf-8")

    code = main(["report", str(results), str(other), "--out-dir", str(tmp_path / "report")])

    assert code == 1
    assert "Golden Set" in capsys.readouterr().err
    assert not (tmp_path / "report").exists()
