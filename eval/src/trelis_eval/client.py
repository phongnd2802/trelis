"""A small client for the Trelis public HTTP API (ADR-0001).

The client never touches Postgres, Go internals or the admin CLI: it acts as
one Principal with that Principal's API key, exactly as any other client does.
"""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any
from urllib.parse import quote

import httpx

from ._fields import (
    float_field,
    float_map_field,
    int_field,
    optional_int_field,
    optional_string_field,
    string_field,
)


class TrelisError(Exception):
    """A failed API call: a non-2xx answer or a malformed success body.

    ``code`` is the numeric application code from the error envelope, the
    string code from a duplicate-upload answer, or None when the body carried
    neither (the router's own 401, for example).
    """

    def __init__(self, status: int, code: int | str | None, message: str):
        self.status = status
        self.code = code
        self.message = message
        detail = f"HTTP {status}"
        if code is not None:
            detail += f" (code {code})"
        super().__init__(f"{detail}: {message}")


@dataclass(frozen=True)
class Document:
    """The Document fields the harness needs from the API."""

    id: str
    title: str
    file_name: str
    parse_status: str
    parse_error: str = ""

    @classmethod
    def from_json(cls, value: Any) -> Document:
        """Read one Document JSON object, rejecting a malformed one."""
        if not isinstance(value, dict):
            raise ValueError("Document payload must be a JSON object")
        fields: dict[str, str] = {}
        for name in ("id", "title", "file_name", "parse_status"):
            field = value.get(name)
            if not isinstance(field, str):
                raise ValueError(f"field {name!r} must be a string")
            fields[name] = field
        parse_error = value.get("parse_error") or ""
        if not isinstance(parse_error, str):
            raise ValueError("field 'parse_error' must be a string")
        return cls(parse_error=parse_error, **fields)


@dataclass(frozen=True)
class UploadResult:
    """The Document an upload produced or found, and whether it was a duplicate."""

    document: Document
    duplicate: bool


@dataclass(frozen=True)
class RetrievedChunk:
    """One ranked Chunk from POST /retrieve, with its per-technique scores."""

    chunk_id: str
    document_id: str
    seq: int
    content: str
    context_header: str
    score: float
    scores: dict[str, float]

    @classmethod
    def from_json(cls, value: Any) -> RetrievedChunk:
        """Read one retrieved Chunk, rejecting a malformed object."""
        if not isinstance(value, dict):
            raise ValueError("retrieved Chunk must be a JSON object")
        return cls(
            chunk_id=string_field(value, "chunk_id"),
            document_id=string_field(value, "document_id"),
            seq=int_field(value, "seq"),
            content=string_field(value, "content"),
            context_header=string_field(value, "context_header"),
            score=float_field(value, "score"),
            scores=float_map_field(value, "scores"),
        )


@dataclass(frozen=True)
class TraceStep:
    """One timed step of one POST /retrieve call; optional fields depend on the step."""

    step: str
    technique: str | None
    duration_ms: int
    model: str | None
    candidates: int | None

    @classmethod
    def from_json(cls, value: Any) -> TraceStep:
        """Read one trace step, rejecting a malformed object."""
        if not isinstance(value, dict):
            raise ValueError("trace step must be a JSON object")
        return cls(
            step=string_field(value, "step"),
            technique=optional_string_field(value, "technique"),
            duration_ms=int_field(value, "duration_ms"),
            model=optional_string_field(value, "model"),
            candidates=optional_int_field(value, "candidates"),
        )


@dataclass(frozen=True)
class RetrievalAnswer:
    """The ranked Chunks and step trace from POST /retrieve."""

    results: tuple[RetrievedChunk, ...]
    trace: tuple[TraceStep, ...]

    @classmethod
    def from_json(cls, value: Any) -> RetrievalAnswer:
        """Read a retrieval result payload, rejecting a malformed one."""
        if not isinstance(value, dict):
            raise ValueError("retrieval data must be a JSON object")
        raw_results = value.get("results")
        if not isinstance(raw_results, list):
            raise ValueError("field 'results' must be a list")
        raw_trace = value.get("trace")
        if not isinstance(raw_trace, list):
            raise ValueError("field 'trace' must be a list")
        return cls(
            results=tuple(RetrievedChunk.from_json(item) for item in raw_results),
            trace=tuple(TraceStep.from_json(item) for item in raw_trace),
        )


@dataclass(frozen=True)
class Citation:
    """One source a Pipeline Answer cites: the marker and the Chunk it opens."""

    marker: int
    chunk_id: str
    document_id: str

    @classmethod
    def from_json(cls, value: Any) -> Citation:
        """Read one citation, rejecting a malformed object."""
        if not isinstance(value, dict):
            raise ValueError("citation must be a JSON object")
        return cls(
            marker=int_field(value, "marker"),
            chunk_id=string_field(value, "chunk_id"),
            document_id=string_field(value, "document_id"),
        )


@dataclass(frozen=True)
class Usage:
    """The chat call's token accounting from POST /answer."""

    prompt_tokens: int
    completion_tokens: int
    total_tokens: int

    @classmethod
    def from_json(cls, value: Any) -> Usage:
        """Read one usage object, rejecting a malformed one."""
        if not isinstance(value, dict):
            raise ValueError("usage must be a JSON object")
        return cls(
            prompt_tokens=int_field(value, "prompt_tokens"),
            completion_tokens=int_field(value, "completion_tokens"),
            total_tokens=int_field(value, "total_tokens"),
        )


@dataclass(frozen=True)
class AnswerDebug:
    """What POST /answer with debug adds: the retrieved Chunks, trace and usage."""

    results: tuple[RetrievedChunk, ...]
    trace: tuple[TraceStep, ...]
    usage: Usage

    @classmethod
    def from_json(cls, value: Any) -> AnswerDebug:
        """Read a debug object, rejecting a malformed one."""
        if not isinstance(value, dict):
            raise ValueError("debug data must be a JSON object")
        raw_results = value.get("results")
        if not isinstance(raw_results, list):
            raise ValueError("field 'debug.results' must be a list")
        raw_trace = value.get("trace")
        if not isinstance(raw_trace, list):
            raise ValueError("field 'debug.trace' must be a list")
        return cls(
            results=tuple(RetrievedChunk.from_json(item) for item in raw_results),
            trace=tuple(TraceStep.from_json(item) for item in raw_trace),
            usage=Usage.from_json(value.get("usage")),
        )


@dataclass(frozen=True)
class AnswerResult:
    """The answer text, its citations, the finish reason and the debug object."""

    answer: str
    citations: tuple[Citation, ...]
    finish_reason: str
    debug: AnswerDebug | None

    @classmethod
    def from_json(cls, value: Any) -> AnswerResult:
        """Read an answer payload, rejecting a malformed one."""
        if not isinstance(value, dict):
            raise ValueError("answer data must be a JSON object")
        raw_citations = value.get("citations")
        if not isinstance(raw_citations, list):
            raise ValueError("field 'citations' must be a list")
        raw_debug = value.get("debug")
        debug = None if raw_debug is None else AnswerDebug.from_json(raw_debug)
        return cls(
            answer=string_field(value, "answer"),
            citations=tuple(Citation.from_json(item) for item in raw_citations),
            finish_reason=string_field(value, "finish_reason"),
            debug=debug,
        )


class TrelisClient:
    """Calls Trelis as one Principal, authenticated with that Principal's API key."""

    def __init__(
        self,
        base_url: str,
        api_key: str,
        *,
        client: httpx.Client | None = None,
        transport: httpx.BaseTransport | None = None,
        timeout: float = 30.0,
    ):
        if not api_key:
            raise ValueError("an API key is required")
        if client is not None and transport is not None:
            raise ValueError("pass either an httpx.Client or a transport, not both")
        self._base_url = base_url.rstrip("/")
        self._headers = {"Authorization": f"Bearer {api_key}"}
        self._owns_client = client is None
        self._client = client or httpx.Client(transport=transport, timeout=timeout)

    def __enter__(self) -> TrelisClient:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def close(self) -> None:
        """Close the underlying HTTP client when this object created it."""
        if self._owns_client:
            self._client.close()

    def me(self) -> dict[str, Any]:
        """Return the Principal this client acts as, from GET /me."""
        response = self._request("GET", "/me")
        body = self._success_json(response)
        if not isinstance(body, dict):
            raise TrelisError(response.status_code, None, "malformed /me response")
        return body

    def upload_document(
        self,
        file_name: str,
        content: bytes,
        principals: Sequence[str] = (),
        groups: Sequence[str] = (),
    ) -> UploadResult:
        """Upload one Source File, returning the earlier Document on a 409."""
        form: dict[str, str] = {}
        if principals:
            form["principals"] = ",".join(principals)
        if groups:
            form["groups"] = ",".join(groups)
        response = self._request(
            "POST",
            "/documents",
            files={"file": (file_name, content, "text/markdown")},
            data=form,
        )
        if response.status_code == httpx.codes.CONFLICT:
            body = _json(response)
            if isinstance(body, dict) and body.get("code") == "duplicate_document":
                return UploadResult(
                    document=self._document(body.get("data"), response.status_code),
                    duplicate=True,
                )
        data = self._data(response)
        return UploadResult(document=self._document(data, response.status_code), duplicate=False)

    def get_document(self, document_id: str) -> Document:
        """Read one Document's current parse state from GET /documents/{id}."""
        response = self._request("GET", f"/documents/{quote(document_id, safe='')}")
        return self._document(self._data(response), response.status_code)

    def retrieve(self, query: str, top_k: int, techniques: Sequence[str]) -> RetrievalAnswer:
        """Run the named Retrieval Techniques over POST /retrieve."""
        response = self._request(
            "POST",
            "/retrieve",
            json={
                "query": query,
                "top_k": top_k,
                "retrieval_config": {"techniques": list(techniques)},
            },
        )
        data = self._data(response)
        try:
            return RetrievalAnswer.from_json(data)
        except ValueError as exc:
            raise TrelisError(
                response.status_code, None, f"malformed retrieval answer: {exc}"
            ) from exc

    def answer(
        self,
        query: str,
        top_k: int,
        techniques: Sequence[str],
        debug: bool = True,
    ) -> AnswerResult:
        """Run the Pipeline Answer over POST /answer, optionally with its debug object."""
        response = self._request(
            "POST",
            "/answer",
            json={
                "query": query,
                "top_k": top_k,
                "retrieval_config": {"techniques": list(techniques)},
                "debug": debug,
            },
        )
        data = self._data(response)
        try:
            result = AnswerResult.from_json(data)
        except ValueError as exc:
            raise TrelisError(response.status_code, None, f"malformed answer: {exc}") from exc
        if debug and result.debug is None:
            raise TrelisError(
                response.status_code,
                None,
                "malformed answer: field 'debug' is missing although the request asked for it",
            )
        return result

    def _request(self, method: str, path: str, **kwargs: Any) -> httpx.Response:
        return self._client.request(
            method, f"{self._base_url}{path}", headers=self._headers, **kwargs
        )

    def _success_json(self, response: httpx.Response) -> Any:
        self._raise_for_response(response)
        body = _json(response)
        if body is None:
            raise TrelisError(response.status_code, None, "response body is not JSON")
        return body

    def _data(self, response: httpx.Response) -> Any:
        body = self._success_json(response)
        if not isinstance(body, dict) or body.get("success") is not True or "data" not in body:
            raise TrelisError(response.status_code, None, "malformed response envelope")
        return body["data"]

    def _document(self, value: Any, status: int) -> Document:
        try:
            return Document.from_json(value)
        except ValueError as exc:
            raise TrelisError(status, None, f"malformed Document: {exc}") from exc

    @staticmethod
    def _raise_for_response(response: httpx.Response) -> None:
        if response.is_success:
            return
        body = _json(response)
        code: int | str | None = None
        message = f"request failed with HTTP {response.status_code}"
        if isinstance(body, dict):
            error = body.get("error")
            if isinstance(error, dict):
                code = error.get("code")
                if isinstance(error.get("message"), str):
                    message = error["message"]
            elif isinstance(error, str):
                message = error
            elif isinstance(body.get("message"), str):
                message = body["message"]
        raise TrelisError(response.status_code, code, message)


def _json(response: httpx.Response) -> Any:
    """Decode a response body, returning None when it is not JSON."""
    try:
        return response.json()
    except ValueError:
        return None
