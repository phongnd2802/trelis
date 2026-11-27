"""Tests for the public HTTP API client, using MockTransport (no server)."""

import httpx
import pytest

from trelis_eval.client import Document, TrelisClient, TrelisError

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