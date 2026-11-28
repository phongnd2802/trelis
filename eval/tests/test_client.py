"""Tests for the public HTTP API client, using MockTransport (no server)."""

import json

import httpx
import pytest

from trelis_eval.client import (
    AnswerDebug,
    AnswerResult,
    Citation,
    Document,
    RetrievalAnswer,
    RetrievedChunk,
    TraceStep,
    TrelisClient,
    TrelisError,
    Usage,
)

BASE_URL = "http://trelis.test"
API_KEY = "test-key-never-logged"


def document(**overrides):
    value = {
        "id": "doc-1",
        "title": "Alpha Mission Launches",
        "file_name": "alpha-mission-launches-1234abcd.md",
        "parse_status": "pending",
        "parse_error": "",
    }
    value.update(overrides)
    return value


def make_client(handler):
    return TrelisClient(BASE_URL, API_KEY, transport=httpx.MockTransport(handler))


def test_me_sends_the_bearer_key_and_reads_the_bare_principal():
    seen = {}

    def handler(request):
        seen["authorization"] = request.headers["authorization"]
        seen["method"] = request.method
        seen["url"] = str(request.url)
        return httpx.Response(200, json={"id": "p1", "name": "alice", "groups": []})

    with make_client(handler) as client:
        principal = client.me()

    assert principal == {"id": "p1", "name": "alice", "groups": []}
    assert seen == {
        "authorization": f"Bearer {API_KEY}",
        "method": "GET",
        "url": f"{BASE_URL}/me",
    }


def test_upload_document_sends_multipart_file_and_comma_joined_access_list():
    seen = {}

    def handler(request):
        seen["content_type"] = request.headers["content-type"]
        seen["body"] = request.content
        return httpx.Response(200, json={"success": True, "data": document()})

    result = make_client(handler).upload_document(
        "alpha.md", b"# Alpha\n", principals=["alice", "bob"], groups=["team"]
    )

    assert result.document == Document(
        id="doc-1",
        title="Alpha Mission Launches",
        file_name="alpha-mission-launches-1234abcd.md",
        parse_status="pending",
    )
    assert result.duplicate is False
    assert seen["content_type"].startswith("multipart/form-data; boundary=")
    assert b'name="file"; filename="alpha.md"' in seen["body"]
    assert b"# Alpha" in seen["body"]
    assert b'name="principals"' in seen["body"]
    assert b"alice,bob" in seen["body"]
    assert b'name="groups"' in seen["body"]
    assert b"team" in seen["body"]


def test_upload_document_omits_empty_access_list_fields():
    seen = {}

    def handler(request):
        seen["body"] = request.content
        return httpx.Response(200, json={"success": True, "data": document()})

    make_client(handler).upload_document("alpha.md", b"# Alpha\n")

    assert b'name="principals"' not in seen["body"]
    assert b'name="groups"' not in seen["body"]


def test_duplicate_upload_returns_the_earlier_document_flagged():
    earlier = document(id="doc-earlier", parse_status="completed")

    def handler(request):
        return httpx.Response(
            409,
            json={
                "success": False,
                "message": "Document already exists: alpha.md",
                "data": earlier,
                "code": "duplicate_document",
            },
        )

    result = make_client(handler).upload_document("alpha.md", b"# Alpha\n")

    assert result.duplicate is True
    assert result.document == Document(
        id="doc-earlier",
        title="Alpha Mission Launches",
        file_name="alpha-mission-launches-1234abcd.md",
        parse_status="completed",
    )


def test_get_document_reads_parse_state():
    def handler(request):
        assert request.method == "GET"
        assert str(request.url) == f"{BASE_URL}/documents/doc-1"
        return httpx.Response(
            200,
            json={
                "success": True,
                "data": document(parse_status="failed", parse_error="parse: unsupported file"),
            },
        )

    fetched = make_client(handler).get_document("doc-1")

    assert fetched.parse_status == "failed"
    assert fetched.parse_error == "parse: unsupported file"


def test_application_error_envelope_becomes_trelis_error():
    def handler(request):
        return httpx.Response(
            404,
            json={
                "success": False,
                "error": {"code": 1004, "message": "No readable Document", "details": None},
            },
        )

    with pytest.raises(TrelisError) as caught:
        make_client(handler).get_document("missing")

    assert caught.value.status == 404
    assert caught.value.code == 1004
    assert caught.value.message == "No readable Document"
    assert API_KEY not in str(caught.value)


def test_router_error_body_becomes_trelis_error_without_a_code():
    def handler(request):
        return httpx.Response(401, json={"error": "invalid API key"})

    with pytest.raises(TrelisError) as caught:
        make_client(handler).me()

    assert caught.value.status == 401
    assert caught.value.code is None
    assert caught.value.message == "invalid API key"
    assert API_KEY not in str(caught.value)


def test_a_malformed_success_envelope_is_an_error():
    def handler(request):
        return httpx.Response(200, json={"data": document()})

    with pytest.raises(TrelisError, match="malformed response envelope"):
        make_client(handler).get_document("doc-1")


def retrieval_data():
    return {
        "results": [
            {
                "chunk_id": "chunk-1",
                "document_id": "doc-1",
                "seq": 0,
                "content": "The Alpha Mission launched in 2024.",
                "context_header": "# Alpha Mission Launches",
                "score": 0.75,
                "scores": {"dense": 0.75},
            },
            {
                "chunk_id": "chunk-2",
                "document_id": "doc-2",
                "seq": 3,
                "content": "The Beta Probe returned in 2024.",
                "context_header": "",
                "score": 0.5,
                "scores": {"dense": 0.5},
            },
        ],
        "trace": [
            {
                "step": "embed_query",
                "technique": "dense",
                "duration_ms": 4,
                "model": "text-embedding-3-small",
            },
            {"step": "search", "technique": "dense", "duration_ms": 6, "candidates": 2},
        ],
    }


def test_retrieve_posts_the_query_top_k_and_techniques_and_parses_the_answer():
    seen = {}

    def handler(request):
        seen["method"] = request.method
        seen["url"] = str(request.url)
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={"success": True, "data": retrieval_data()})

    answer = make_client(handler).retrieve("Which missions?", 5, ["dense", "bm25"])

    assert seen == {
        "method": "POST",
        "url": f"{BASE_URL}/retrieve",
        "body": {
            "query": "Which missions?",
            "top_k": 5,
            "retrieval_config": {"techniques": ["dense", "bm25"]},
        },
    }
    assert answer == RetrievalAnswer(
        results=(
            RetrievedChunk(
                chunk_id="chunk-1",
                document_id="doc-1",
                seq=0,
                content="The Alpha Mission launched in 2024.",
                context_header="# Alpha Mission Launches",
                score=0.75,
                scores={"dense": 0.75},
            ),
            RetrievedChunk(
                chunk_id="chunk-2",
                document_id="doc-2",
                seq=3,
                content="The Beta Probe returned in 2024.",
                context_header="",
                score=0.5,
                scores={"dense": 0.5},
            ),
        ),
        trace=(
            TraceStep(
                step="embed_query",
                technique="dense",
                duration_ms=4,
                model="text-embedding-3-small",
                candidates=None,
            ),
            TraceStep(
                step="search",
                technique="dense",
                duration_ms=6,
                model=None,
                candidates=2,
            ),
        ),
    )


def test_retrieve_reports_an_unimplemented_technique_as_the_validation_error():
    def handler(request):
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

    with pytest.raises(TrelisError) as caught:
        make_client(handler).retrieve("Which?", 10, ["bm25"])

    assert caught.value.status == 400
    assert caught.value.code == 1010
    assert "bm25" in caught.value.message


@pytest.mark.parametrize(
    "data",
    [
        {"results": "nope", "trace": []},
        {"results": [{"chunk_id": 1}], "trace": []},
        {
            "results": [
                {
                    "chunk_id": "chunk-1",
                    "document_id": "doc-1",
                    "seq": 0,
                    "content": "text",
                    "context_header": "",
                    "score": "high",
                    "scores": {},
                }
            ],
            "trace": [],
        },
        {
            "results": [
                {
                    "chunk_id": "chunk-1",
                    "document_id": "doc-1",
                    "seq": 0,
                    "content": "text",
                    "context_header": "",
                    "score": 1.0,
                    "scores": [],
                }
            ],
            "trace": [],
        },
        {"results": [], "trace": [{"step": "search", "duration_ms": "slow"}]},
    ],
)
def test_retrieve_rejects_a_malformed_answer(data):
    def handler(request):
        return httpx.Response(200, json={"success": True, "data": data})

    with pytest.raises(TrelisError, match="malformed retrieval answer"):
        make_client(handler).retrieve("Which?", 10, ["dense"])


def test_retrieve_accepts_a_trace_step_without_optional_fields():
    def handler(request):
        return httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "results": [],
                    "trace": [{"step": "search", "duration_ms": 1}],
                },
            },
        )

    answer = make_client(handler).retrieve("Which?", 10, ["dense"])

    assert answer.trace == (
        TraceStep(step="search", technique=None, duration_ms=1, model=None, candidates=None),
    )


def answer_data():
    data = retrieval_data()
    return {
        "answer": "The Alpha Mission launched in 2024 [1].",
        "citations": [{"marker": 1, "chunk_id": "chunk-1", "document_id": "doc-1"}],
        "finish_reason": "stop",
        "debug": {
            "results": data["results"],
            "trace": data["trace"],
            "usage": {"prompt_tokens": 120, "completion_tokens": 20, "total_tokens": 140},
        },
    }


def test_answer_posts_the_query_with_debug_and_parses_the_answer():
    seen = {}

    def handler(request):
        seen["method"] = request.method
        seen["url"] = str(request.url)
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={"success": True, "data": answer_data()})

    result = make_client(handler).answer("Which missions?", 5, ["dense", "bm25"])

    assert seen == {
        "method": "POST",
        "url": f"{BASE_URL}/answer",
        "body": {
            "query": "Which missions?",
            "top_k": 5,
            "retrieval_config": {"techniques": ["dense", "bm25"]},
            "debug": True,
        },
    }
    assert result == AnswerResult(
        answer="The Alpha Mission launched in 2024 [1].",
        citations=(Citation(marker=1, chunk_id="chunk-1", document_id="doc-1"),),
        finish_reason="stop",
        debug=AnswerDebug(
            results=(
                RetrievedChunk(
                    chunk_id="chunk-1",
                    document_id="doc-1",
                    seq=0,
                    content="The Alpha Mission launched in 2024.",
                    context_header="# Alpha Mission Launches",
                    score=0.75,
                    scores={"dense": 0.75},
                ),
                RetrievedChunk(
                    chunk_id="chunk-2",
                    document_id="doc-2",
                    seq=3,
                    content="The Beta Probe returned in 2024.",
                    context_header="",
                    score=0.5,
                    scores={"dense": 0.5},
                ),
            ),
            trace=(
                TraceStep(
                    step="embed_query",
                    technique="dense",
                    duration_ms=4,
                    model="text-embedding-3-small",
                    candidates=None,
                ),
                TraceStep(
                    step="search",
                    technique="dense",
                    duration_ms=6,
                    model=None,
                    candidates=2,
                ),
            ),
            usage=Usage(prompt_tokens=120, completion_tokens=20, total_tokens=140),
        ),
    )


def test_answer_without_debug_keeps_the_debug_object_absent():
    data = answer_data()
    del data["debug"]

    def handler(request):
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={"success": True, "data": data})

    seen = {}

    result = make_client(handler).answer("Which?", 10, ["dense"], debug=False)

    assert seen["body"]["debug"] is False
    assert result.debug is None


def test_answer_raises_trelis_error_like_retrieve():
    def handler(request):
        return httpx.Response(
            400,
            json={
                "success": False,
                "error": {"code": 1010, "message": "bad request", "details": None},
            },
        )

    with pytest.raises(TrelisError) as caught:
        make_client(handler).answer("Which?", 10, ["dense"])

    assert caught.value.status == 400
    assert caught.value.code == 1010


@pytest.mark.parametrize(
    "data",
    [
        {**answer_data(), "answer": None},
        {**answer_data(), "citations": "nope"},
        {**answer_data(), "finish_reason": None},
        {
            **answer_data(),
            "debug": {
                "results": [],
                "trace": [],
                "usage": {"prompt_tokens": "many"},
            },
        },
    ],
)
def test_answer_rejects_a_malformed_answer(data):
    def handler(request):
        return httpx.Response(200, json={"success": True, "data": data})

    with pytest.raises(TrelisError, match="malformed answer"):
        make_client(handler).answer("Which?", 10, ["dense"])


def test_answer_requires_the_debug_object_when_it_asked_for_one():
    data = answer_data()
    del data["debug"]

    def handler(request):
        return httpx.Response(200, json={"success": True, "data": data})

    with pytest.raises(TrelisError, match="debug"):
        make_client(handler).answer("Which?", 10, ["dense"])
