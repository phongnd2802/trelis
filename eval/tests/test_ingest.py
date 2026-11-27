"""Tests for ingesting a Corpus and polling Documents to a final state."""

import json
from pathlib import Path

import httpx
import pytest

from trelis_eval.client import TrelisClient
from trelis_eval.ingest import IngestRecord, IngestStatus, ingest, write_manifest
from trelis_eval.multihop import load_corpus

FIXTURES = Path(__file__).parent / "fixtures"
BASE_URL = "http://trelis.test"


def document_response(document_id, status, parse_error=""):
    return httpx.Response(
        200,
        json={
            "success": True,
            "data": {
                "id": document_id,
                "title": "ignored",
                "file_name": "ignored.md",
                "parse_status": status,
                "parse_error": parse_error,
            },
        },
    )


def server_error():
    return httpx.Response(
        500,
        json={"success": False, "error": {"code": 1000, "message": "boom"}},
    )


class FakeTrelis:
    """A MockTransport handler whose Documents move through scripted states."""

    def __init__(self, states):
        self.states = states
        self.uploads = []
        self.gets = []

    def __call__(self, request):
        if request.method == "POST":
            document_id = f"doc-{len(self.uploads) + 1}"
            self.uploads.append(document_id)
            return document_response(document_id, "pending")
        document_id = request.url.path.rsplit("/", 1)[-1]
        self.gets.append(document_id)
        statuses = self.states[document_id]
        status = statuses.pop(0) if len(statuses) > 1 else statuses[0]
        return document_response(document_id, status, "parse: broken" if status == "failed" else "")


def make_client(handler):
    return TrelisClient(BASE_URL, "test-key", transport=httpx.MockTransport(handler))


def test_ingest_polls_until_every_document_completes():
    fake = FakeTrelis({"doc-1": ["processing", "completed"]})
    documents = load_corpus(FIXTURES / "corpus.json")[:1]

    with make_client(fake) as client:
        records = ingest(
            client,
            documents,
            principals=["alice"],
            groups=[],
            poll_interval=0,
            timeout=5,
            clock=lambda: 0,
            sleep=lambda _: None,
        )

    assert len(fake.gets) == 2
    assert records == [
        IngestRecord(
            source_id="https://example.com/alpha",
            document_id="doc-1",
            file_name=records[0].file_name,
            status=IngestStatus.COMPLETED,
            duplicate=False,
            error="",
        )
    ]
    assert records[0].file_name.endswith(".md")


def test_ingest_records_the_parse_error_of_a_failed_document():
    fake = FakeTrelis({"doc-1": ["failed"]})
    documents = load_corpus(FIXTURES / "corpus.json")[:1]

    with make_client(fake) as client:
        [record] = ingest(
            client,
            documents,
            principals=[],
            groups=[],
            poll_interval=0,
            timeout=5,
            clock=lambda: 0,
            sleep=lambda _: None,
        )

    assert record.status == IngestStatus.FAILED
    assert record.error == "parse: broken"


def test_ingest_times_out_with_an_injected_clock():
    fake = FakeTrelis({"doc-1": ["processing"]})
    documents = load_corpus(FIXTURES / "corpus.json")[:1]
    now = 0

    def clock():
        nonlocal now
        value = now
        now += 1
        return value

    with make_client(fake) as client:
        [record] = ingest(
            client,
            documents,
            principals=[],
            groups=[],
            poll_interval=0,
            timeout=2,
            clock=clock,
            sleep=lambda _: None,
        )

    assert record.status == IngestStatus.TIMEOUT
    assert record.error == "still processing after 2s"
    assert len(fake.gets) == 2


def test_ingest_records_an_upload_error_and_keeps_going():
    posts = 0

    def handler(request):
        nonlocal posts
        if request.method == "POST":
            posts += 1
            if posts == 1:
                return server_error()
            return document_response("doc-1", "pending")
        return document_response("doc-1", "completed")

    documents = load_corpus(FIXTURES / "corpus.json")[:2]

    with make_client(handler) as client:
        records = ingest(
            client,
            documents,
            principals=[],
            groups=[],
            poll_interval=0,
            timeout=5,
            clock=lambda: 0,
            sleep=lambda _: None,
        )

    assert records[0].status == IngestStatus.ERROR
    assert records[0].document_id is None
    assert "HTTP 500" in records[0].error
    assert records[1].status == IngestStatus.COMPLETED


def test_ingest_records_a_transport_error_on_upload():
    def handler(request):
        raise httpx.ConnectError("connection refused")

    documents = load_corpus(FIXTURES / "corpus.json")[:1]

    with make_client(handler) as client:
        [record] = ingest(
            client,
            documents,
            principals=[],
            groups=[],
            poll_interval=0,
            timeout=5,
            clock=lambda: 0,
            sleep=lambda _: None,
        )

    assert record.status == IngestStatus.ERROR
    assert record.document_id is None
    assert "connection refused" in record.error


def test_ingest_retries_a_poll_after_an_http_error():
    gets = 0

    def handler(request):
        nonlocal gets
        if request.method == "POST":
            return document_response("doc-1", "pending")
        gets += 1
        if gets == 1:
            return server_error()
        return document_response("doc-1", "completed")

    documents = load_corpus(FIXTURES / "corpus.json")[:1]

    with make_client(handler) as client:
        [record] = ingest(
            client,
            documents,
            principals=[],
            groups=[],
            poll_interval=0,
            timeout=5,
            clock=lambda: 0,
            sleep=lambda _: None,
        )

    assert gets == 2
    assert record.status == IngestStatus.COMPLETED
    assert record.error == ""


def test_ingest_times_out_when_polls_keep_failing():
    gets = 0

    def handler(request):
        nonlocal gets
        if request.method == "POST":
            return document_response("doc-1", "pending")
        gets += 1
        return server_error()

    documents = load_corpus(FIXTURES / "corpus.json")[:1]
    now = 0

    def clock():
        nonlocal now
        value = now
        now += 1
        return value

    with make_client(handler) as client:
        [record] = ingest(
            client,
            documents,
            principals=[],
            groups=[],
            poll_interval=0,
            timeout=2,
            clock=clock,
            sleep=lambda _: None,
        )

    assert gets == 2
    assert record.status == IngestStatus.TIMEOUT
    assert "HTTP 500" in record.error
    assert "still pending after 2s" in record.error


def test_ingest_keeps_the_duplicate_flag_of_an_earlier_document():
    def handler(request):
        if request.method == "POST":
            return httpx.Response(
                409,
                json={
                    "success": False,
                    "message": "Document already exists",
                    "code": "duplicate_document",
                    "data": {
                        "id": "doc-earlier",
                        "title": "ignored",
                        "file_name": "ignored.md",
                        "parse_status": "completed",
                        "parse_error": "",
                    },
                },
            )
        pytest.fail("a completed duplicate needs no polling")

    documents = load_corpus(FIXTURES / "corpus.json")[:1]

    with make_client(handler) as client:
        [record] = ingest(
            client,
            documents,
            principals=[],
            groups=[],
            poll_interval=0,
            timeout=5,
            clock=lambda: 0,
            sleep=lambda _: None,
        )

    assert record.document_id == "doc-earlier"
    assert record.status == IngestStatus.COMPLETED
    assert record.duplicate is True


def test_write_manifest_writes_one_json_object_per_record(tmp_path):
    records = [
        IngestRecord(
            "https://example.com/alpha",
            "doc-1",
            "alpha.md",
            IngestStatus.COMPLETED,
            False,
            "",
        ),
        IngestRecord(
            "https://example.com/beta",
            "doc-2",
            "beta.md",
            IngestStatus.FAILED,
            True,
            "parse: broken",
        ),
        IngestRecord(
            "https://example.com/gamma",
            None,
            "gamma.md",
            IngestStatus.ERROR,
            False,
            "HTTP 500: request failed",
        ),
    ]
    path = tmp_path / "nested" / "manifest.jsonl"

    write_manifest(path, records)

    lines = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
    assert lines == [
        {
            "source_id": "https://example.com/alpha",
            "document_id": "doc-1",
            "file_name": "alpha.md",
            "status": "completed",
            "duplicate": False,
            "error": "",
        },
        {
            "source_id": "https://example.com/beta",
            "document_id": "doc-2",
            "file_name": "beta.md",
            "status": "failed",
            "duplicate": True,
            "error": "parse: broken",
        },
        {
            "source_id": "https://example.com/gamma",
            "document_id": None,
            "file_name": "gamma.md",
            "status": "error",
            "duplicate": False,
            "error": "HTTP 500: request failed",
        },
    ]
