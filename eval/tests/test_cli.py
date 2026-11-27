"""Tests for the trelis-eval command line."""

import json
from pathlib import Path

import httpx
import pytest

from trelis_eval.cli import main
from trelis_eval.golden import load_golden_set

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


def test_multihop_ingest_writes_the_subset_golden_set_and_manifest(
    tmp_path, monkeypatch, capsys
):
    server = IngestServer()

    code, golden_out, manifest_out = run_ingest(
        tmp_path, monkeypatch, httpx.MockTransport(server)
    )

    assert code == 0
    golden = load_golden_set(golden_out)
    assert [question.id for question in golden] == ["mhr-0000", "mhr-0001"]
    manifest = [
        json.loads(line)
        for line in manifest_out.read_text(encoding="utf-8").splitlines()
    ]
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

    code, _, manifest_out = run_ingest(
        tmp_path, monkeypatch, httpx.MockTransport(server)
    )

    assert code == 1
    manifest = [
        json.loads(line)
        for line in manifest_out.read_text(encoding="utf-8").splitlines()
    ]
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
    manifest = [
        json.loads(line)
        for line in manifest_out.read_text(encoding="utf-8").splitlines()
    ]
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