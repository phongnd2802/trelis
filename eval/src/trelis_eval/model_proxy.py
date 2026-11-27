"""An OpenAI-compatible record/replay proxy for model calls (ADR-0001).

The Go service has no cache and knows nothing about this proxy: to it the
proxy is just another provider. Every role posts to ``{base_url}/chat/
completions``, ``/responses``, ``/embeddings`` or ``/rerank``, so the proxy
routes by the first path segment and one port can serve every role:

    TRELIS_MODELS_EMBEDDING_BASE_URL=http://127.0.0.1:4100/embedding

A call is keyed by the role, the path after it and the normalised request
body, never by a header, so the same question costs money once. Fixtures are
plain JSON under ``<fixtures-dir>/<role>/<key[:2]>/<key>.json`` and hold the
response bytes base64-encoded. A streamed answer is stored as the chunks the
proxy read from the socket, in order: TCP may merge the provider's writes, so
the entries need not match those writes one-for-one, but the concatenated
bytes, their order and the ``usage`` inside them survive replay exactly.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
import os
import re
import sys
import tempfile
import threading
from collections.abc import Iterable, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from enum import StrEnum
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

import httpx

SCHEMA_VERSION = 1
"""The fixture document layout this build reads and writes."""

UPSTREAM_TIMEOUT = 300.0
"""Seconds one paid model call may take; a long answer must not be cut off."""

_ROLE_RE = re.compile(r"[a-z]+")
_FORWARDED_HEADERS = ("Authorization", "Content-Type", "Accept")
"""The request headers the proxy copies upstream. Hop-by-hop ones never go."""

_DROPPED_RESPONSE_HEADERS = frozenset(
    {
        "connection",
        "keep-alive",
        "transfer-encoding",
        "te",
        "trailer",
        "upgrade",
        "content-length",
        "content-encoding",
    }
)
"""Response headers the proxy never copies: hop-by-hop ones, the framing it
rewrites, and Content-Encoding because bodies are stored decoded. ``Proxy-*``
headers are dropped too."""


class ProxyMode(StrEnum):
    """How the proxy answers one model call."""

    RECORD = "record"
    REPLAY = "replay"
    PASSTHROUGH = "passthrough"


class FixtureError(RuntimeError):
    """A fixture could not be read, written or understood."""


@dataclass(frozen=True)
class RecordedResponse:
    """The upstream answer a fixture replays, byte for byte."""

    status: int
    content_type: str
    stream: bool
    chunks: tuple[bytes, ...]

    def to_json(self) -> dict[str, Any]:
        """Render the response as the fixture document's ``response`` field."""
        return {
            "status": self.status,
            "content_type": self.content_type,
            "stream": self.stream,
            "chunks_b64": [base64.b64encode(chunk).decode("ascii") for chunk in self.chunks],
        }

    @classmethod
    def from_json(cls, value: Any) -> RecordedResponse:
        """Read one fixture's ``response`` field, rejecting a malformed one."""
        if not isinstance(value, dict):
            raise FixtureError("response must be a JSON object")
        status = value.get("status")
        if not isinstance(status, int) or isinstance(status, bool):
            raise FixtureError("response.status must be an integer")
        content_type = value.get("content_type")
        if not isinstance(content_type, str):
            raise FixtureError("response.content_type must be a string")
        stream = value.get("stream")
        if not isinstance(stream, bool):
            raise FixtureError("response.stream must be a boolean")
        chunks = value.get("chunks_b64")
        if not isinstance(chunks, list) or not all(isinstance(chunk, str) for chunk in chunks):
            raise FixtureError("response.chunks_b64 must be a list of strings")
        decoded: list[bytes] = []
        for chunk in chunks:
            try:
                decoded.append(base64.b64decode(chunk, validate=True))
            except (binascii.Error, ValueError) as exc:
                raise FixtureError(f"response.chunks_b64 holds invalid base64: {exc}") from exc
        return cls(
            status=status,
            content_type=content_type,
            stream=stream,
            chunks=tuple(decoded),
        )


@dataclass(frozen=True)
class Fixture:
    """One recorded model call and the answer replayed for it."""

    key: str
    role: str
    path: str
    request: Any
    response: RecordedResponse
    recorded_at: str

    def to_json(self) -> dict[str, Any]:
        """Render the fixture document written to disk."""
        return {
            "schema_version": SCHEMA_VERSION,
            "key": self.key,
            "role": self.role,
            "path": self.path,
            "request": self.request,
            "response": self.response.to_json(),
            "recorded_at": self.recorded_at,
        }

    @classmethod
    def from_json(cls, value: Any) -> Fixture:
        """Read one fixture document, rejecting a malformed one."""
        if not isinstance(value, dict):
            raise FixtureError("fixture must be a JSON object")
        schema_version = value.get("schema_version")
        if schema_version != SCHEMA_VERSION:
            raise FixtureError(
                f"unsupported fixture schema_version {schema_version!r}; want {SCHEMA_VERSION}"
            )
        fields: dict[str, str] = {}
        for name in ("key", "role", "path", "recorded_at"):
            field_value = value.get(name)
            if not isinstance(field_value, str):
                raise FixtureError(f"fixture {name} must be a string")
            fields[name] = field_value
        response = RecordedResponse.from_json(value.get("response"))
        return cls(
            key=fields["key"],
            role=fields["role"],
            path=fields["path"],
            request=value.get("request"),
            response=response,
            recorded_at=fields["recorded_at"],
        )


def normalize_body(body: bytes) -> bytes:
    """Return a request body in the form the fixture key hashes.

    A JSON body is re-serialised with sorted keys and no insignificant
    whitespace, so a provider reordering an object or formatting it
    differently does not change the key. Anything that is not JSON is kept
    exactly as the bytes arrived.
    """
    try:
        value = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return body
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode(
        "utf-8"
    )


def fixture_key(role: str, path: str, body: bytes) -> str:
    """The hex sha256 naming one call: role, path after the role, body.

    Role and path cannot contain a newline, so framing them with newlines
    makes the three parts unambiguous without adding a length prefix.
    """
    framed = b"\n".join((role.encode("utf-8"), path.encode("utf-8"), normalize_body(body)))
    return hashlib.sha256(framed).hexdigest()


def _request_document(body: bytes) -> Any:
    """The request body as stored in a fixture: JSON, or base64 when it is not."""
    try:
        return json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return {"raw_b64": base64.b64encode(body).decode("ascii")}


def _request_model(request: Any) -> str | None:
    """The ``model`` a request names, when it is a JSON object that names one."""
    if isinstance(request, dict):
        model = request.get("model")
        if isinstance(model, str):
            return model
    return None


def _utc_now() -> str:
    """The current UTC time in the fixture's timestamp format."""
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


class FixtureStore:
    """The fixture tree: one JSON file per recorded call, plus a miss log."""

    def __init__(self, root: Path):
        self.root = Path(root)
        self._miss_lock = threading.Lock()

    def path_for(self, role: str, key: str) -> Path:
        """The file one fixture lives in."""
        return self.root / role / key[:2] / f"{key}.json"

    def load(self, role: str, key: str) -> Fixture | None:
        """Read one fixture, or None when it was never recorded."""
        path = self.path_for(role, key)
        try:
            text = path.read_text(encoding="utf-8")
        except FileNotFoundError:
            return None
        except OSError as exc:
            raise FixtureError(f"cannot read fixture {path}: {exc}") from exc
        try:
            document = json.loads(text)
        except json.JSONDecodeError as exc:
            raise FixtureError(f"{path} is not valid JSON: {exc}") from exc
        fixture = Fixture.from_json(document)
        if fixture.key != key or fixture.role != role:
            raise FixtureError(f"{path} holds {fixture.role} {fixture.key}, not {role} {key}")
        return fixture

    def save(self, fixture: Fixture) -> Path:
        """Write one fixture atomically, so a threaded reader never sees half."""
        path = self.path_for(fixture.role, fixture.key)
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            text = json.dumps(fixture.to_json(), ensure_ascii=False, indent=2) + "\n"
            descriptor, temporary = tempfile.mkstemp(
                prefix=f".{fixture.key}.", suffix=".tmp", dir=path.parent
            )
            os.close(descriptor)
            try:
                with open(temporary, "w", encoding="utf-8") as handle:
                    handle.write(text)
                os.replace(temporary, path)
            finally:
                try:
                    os.unlink(temporary)
                except FileNotFoundError:
                    pass
        except OSError as exc:
            raise FixtureError(f"cannot save fixture {path}: {exc}") from exc
        return path

    def record_miss(self, role: str, path: str, key: str, request: Any) -> Path:
        """Append one replay miss to ``misses.jsonl`` so its request is visible."""
        miss_path = self.root / "misses.jsonl"
        line = json.dumps(
            {
                "recorded_at": _utc_now(),
                "key": key,
                "role": role,
                "path": path,
                "request": request,
            },
            ensure_ascii=False,
            separators=(",", ":"),
        )
        try:
            self.root.mkdir(parents=True, exist_ok=True)
            with self._miss_lock, miss_path.open("a", encoding="utf-8") as handle:
                handle.write(line + "\n")
        except OSError as exc:
            raise FixtureError(f"cannot record miss in {miss_path}: {exc}") from exc
        return miss_path

    def roles(self) -> tuple[str, ...]:
        """The role directories present, which is how replay knows its roles."""
        try:
            entries = list(self.root.iterdir())
        except FileNotFoundError:
            return ()
        except OSError as exc:
            raise FixtureError(f"cannot list fixtures in {self.root}: {exc}") from exc
        return tuple(
            sorted(
                entry.name for entry in entries if entry.is_dir() and _ROLE_RE.fullmatch(entry.name)
            )
        )


@dataclass(frozen=True)
class ProxyConfig:
    """Everything one proxy run needs: its mode, fixture tree and upstreams."""

    mode: ProxyMode
    fixtures_dir: Path
    upstreams: dict[str, str] = field(default_factory=dict)

    def __post_init__(self) -> None:
        object.__setattr__(self, "mode", ProxyMode(self.mode))
        object.__setattr__(self, "fixtures_dir", Path(self.fixtures_dir))
        object.__setattr__(self, "upstreams", dict(self.upstreams))
        if self.mode is not ProxyMode.REPLAY and not self.upstreams:
            raise ValueError(f"{self.mode.value} mode needs at least one --upstream")


def parse_upstreams(values: Sequence[str]) -> dict[str, str]:
    """Parse repeated ``ROLE=URL`` flags into the map a ProxyConfig takes.

    A role is lower-case letters only and may be named once; a URL must be
    http(s) and a base (no query or fragment), because the request path is
    appended to it. A query would also be a place for a key to leak into
    shell history and logs, so it is refused rather than silently dropped.
    """
    upstreams: dict[str, str] = {}
    for value in values:
        role, separator, url = value.partition("=")
        if not separator:
            raise ValueError(f"--upstream must be ROLE=URL, got {value!r}")
        if not _ROLE_RE.fullmatch(role):
            raise ValueError(f"--upstream role {role!r} must match [a-z]+, e.g. embedding=...")
        if role in upstreams:
            raise ValueError(f"--upstream role {role!r} was given more than once")
        parsed = urlsplit(url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ValueError(f"--upstream {role} URL {url!r} must be an http(s) URL")
        if parsed.username is not None or parsed.password is not None:
            raise ValueError(
                f"--upstream {role} URL must not embed credentials; pass the key through "
                f"TRELIS_MODELS_{role.upper()}_API_KEY"
            )
        if parsed.query or parsed.fragment:
            raise ValueError(
                f"--upstream {role} URL must not carry a query or fragment; got {url!r}"
            )
        upstreams[role] = url.rstrip("/")
    return upstreams


def parse_listen(value: str) -> tuple[str, int]:
    """Split ``HOST:PORT`` into the address ``serve`` binds."""
    host, separator, port_text = value.rpartition(":")
    if not separator or not host:
        raise ValueError(f"--listen must be HOST:PORT, got {value!r}")
    if host.startswith("[") and host.endswith("]"):
        host = host[1:-1]
    try:
        port = int(port_text)
    except ValueError as exc:
        raise ValueError(f"--listen port {port_text!r} is not a number") from exc
    if not 0 <= port <= 65535:
        raise ValueError(f"--listen port {port} must be between 0 and 65535")
    return host, port


def base_url_lines(host: str, port: int, roles: Iterable[str]) -> list[str]:
    """The ``TRELIS_MODELS_<ROLE>_BASE_URL`` lines a developer copies."""
    return [
        f"TRELIS_MODELS_{role.upper()}_BASE_URL=http://{host}:{port}/{role}"
        for role in sorted(roles)
    ]


def serve(
    config: ProxyConfig,
    host: str,
    port: int,
    *,
    client: httpx.Client | None = None,
) -> ThreadingHTTPServer:
    """Bind a proxy server and return it; the caller runs ``serve_forever``.

    Passing port 0 binds a free port, which is how tests start the proxy.
    ``client`` lets a caller share one upstream connection pool; without it
    the server builds and owns its own client and closes it with the socket.
    """
    store = FixtureStore(config.fixtures_dir)
    owns_client = client is None
    if client is None:
        client = httpx.Client(timeout=UPSTREAM_TIMEOUT)
    return _ProxyServer((host, port), config, store, client, owns_client)


def _error(message: str, kind: str) -> dict[str, Any]:
    """The JSON error body every proxy-generated answer uses."""
    return {"error": {"message": message, "type": kind}}


class _BadRequest(Exception):
    """The request cannot be read as an HTTP body the proxy understands."""


class _ProxyServer(ThreadingHTTPServer):
    """A threaded HTTP server carrying the proxy's shared state."""

    daemon_threads = True

    def __init__(
        self,
        address: tuple[str, int],
        config: ProxyConfig,
        store: FixtureStore,
        client: httpx.Client,
        owns_client: bool,
    ):
        super().__init__(address, _ProxyHandler)
        self.config = config
        self.store = store
        self.client = client
        self.owns_client = owns_client

    def server_close(self) -> None:
        """Close the listening socket and the upstream client this server owns."""
        super().server_close()
        if self.owns_client:
            self.client.close()


class _ProxyHandler(BaseHTTPRequestHandler):
    """Routes one model call to a fixture, an upstream, or a replay miss."""

    protocol_version = "HTTP/1.1"

    def do_POST(self) -> None:
        try:
            self._proxy_post()
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True

    def do_GET(self) -> None:
        self._method_not_allowed()

    do_PUT = do_GET
    do_PATCH = do_GET
    do_DELETE = do_GET
    do_HEAD = do_GET
    do_OPTIONS = do_GET

    def _proxy_post(self) -> None:
        config = self.server.config
        try:
            body = self._read_body()
        except _BadRequest as exc:
            # The unread body cannot be framed again, so this connection must
            # not be reused for the next request.
            self.close_connection = True
            self._send_json(400, _error(str(exc), "bad_request"))
            return
        role, path = _split_role(self.path)
        if not self._accepts_role(role):
            configured = ", ".join(sorted(self._configured_roles())) or "(none)"
            self._send_json(
                404,
                _error(
                    f"unknown role {role!r}; configured roles: {configured}",
                    "unknown_role",
                ),
            )
            return

        key = fixture_key(role, path, body)
        request = _request_document(body)
        if config.mode is not ProxyMode.PASSTHROUGH:
            try:
                fixture = self.server.store.load(role, key)
            except FixtureError as exc:
                print(f"proxy: {exc}", file=sys.stderr)
                self._send_json(500, _error(str(exc), "fixture_error"))
                return
            if fixture is not None:
                self._send_fixture(fixture)
                return
        if config.mode is ProxyMode.REPLAY:
            self._replay_miss(role, path, key, request)
            return
        self._forward(role, path, key, body, request, save=config.mode is ProxyMode.RECORD)

    def _accepts_role(self, role: str) -> bool:
        """Whether this role can be routed in the current mode.

        Replay has no upstreams to name roles, so any role shaped like one is
        routed and simply misses when no fixture exists; record and
        passthrough need a ``--upstream`` before they can answer at all.
        """
        if not _ROLE_RE.fullmatch(role):
            return False
        if self.server.config.mode is ProxyMode.REPLAY:
            return True
        return role in self.server.config.upstreams

    def _configured_roles(self) -> set[str]:
        """The roles named in the error for an unknown role."""
        config = self.server.config
        if config.mode is ProxyMode.REPLAY:
            return set(self.server.store.roles()) | set(config.upstreams)
        return set(config.upstreams)

    def _body_length(self) -> int:
        """The request body's length, refusing framing the proxy cannot read."""
        transfer_encoding = self.headers.get("Transfer-Encoding", "").strip().lower()
        if transfer_encoding not in ("", "identity"):
            raise _BadRequest("chunked request bodies are not supported; send Content-Length")
        length_text = self.headers.get("Content-Length", "0").strip() or "0"
        try:
            length = int(length_text)
        except ValueError as exc:
            raise _BadRequest(f"invalid Content-Length {length_text!r}") from exc
        if length < 0:
            raise _BadRequest(f"invalid Content-Length {length}")
        return length

    def _read_body(self) -> bytes:
        length = self._body_length()
        if length == 0:
            return b""
        body = self.rfile.read(length)
        if len(body) != length:
            raise _BadRequest(f"request body ended after {len(body)} of {length} bytes")
        return body

    def _replay_miss(self, role: str, path: str, key: str, request: Any) -> None:
        model = _request_model(request) or "<none>"
        print(
            f"replay miss: role={role} path={path} key={key} model={model}",
            file=sys.stderr,
        )
        try:
            self.server.store.record_miss(role, path, key, request)
        except FixtureError as exc:
            print(f"proxy: {exc}", file=sys.stderr)
        message = (
            f"replay miss: no fixture for {role} {path} (key {key}); re-record with --mode record"
        )
        self._send_json(
            404,
            {"error": {"message": message, "type": "replay_miss", "key": key}},
        )

    def _send_fixture(self, fixture: Fixture) -> None:
        response = fixture.response
        if response.stream:
            self.send_response(response.status)
            self.send_header("Content-Type", response.content_type)
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for chunk in response.chunks:
                self._write_chunk(chunk)
            self._end_chunks()
            return
        self._send_answer(
            response.status,
            [("Content-Type", response.content_type)],
            b"".join(response.chunks),
        )

    def _forward(
        self,
        role: str,
        path: str,
        key: str,
        body: bytes,
        request: Any,
        *,
        save: bool,
    ) -> None:
        url = f"{self.server.config.upstreams[role]}{path}"
        try:
            with self.server.client.stream(
                "POST", url, content=body, headers=self._upstream_headers()
            ) as response:
                if not 200 <= response.status_code < 300:
                    self._relay_error(response)
                elif _is_stream(request, response.headers.get("Content-Type", "")):
                    self._relay_stream(response, key, role, path, request, save=save)
                else:
                    self._relay_body(response, key, role, path, request, save=save)
        except httpx.HTTPError as exc:
            self._send_json(
                502,
                _error(f"upstream request failed: {exc}", "upstream_error"),
            )

    def _upstream_headers(self) -> dict[str, str]:
        """The request headers copied to the provider, and a plain-byte hint."""
        headers = {
            name: value
            for name in _FORWARDED_HEADERS
            if (value := self.headers.get(name)) is not None
        }
        # iter_bytes decodes whatever arrives; asking for identity usually
        # avoids compression in the first place.
        headers["Accept-Encoding"] = "identity"
        return headers

    def _relay_error(self, response: httpx.Response) -> None:
        """Pass a non-2xx answer back with every header a retry may read."""
        payload = b"".join(response.iter_bytes())
        self._send_answer(response.status_code, _relayed_headers(response), payload)

    def _relay_body(
        self,
        response: httpx.Response,
        key: str,
        role: str,
        path: str,
        request: Any,
        *,
        save: bool,
    ) -> None:
        payload = b"".join(response.iter_bytes())
        if save:
            self._save_answer(
                key,
                role,
                path,
                request,
                response,
                stream=False,
                chunks=(payload,),
            )
        self._send_answer(response.status_code, _relayed_headers(response), payload)

    def _relay_stream(
        self,
        response: httpx.Response,
        key: str,
        role: str,
        path: str,
        request: Any,
        *,
        save: bool,
    ) -> None:
        client_gone = False
        try:
            self._start_response(response.status_code, _relayed_headers(response))
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
        except (BrokenPipeError, ConnectionResetError):
            client_gone = True
            self.close_connection = True
        chunks: list[bytes] = []
        try:
            for chunk in response.iter_bytes():
                if not chunk:
                    continue
                chunks.append(chunk)
                if client_gone:
                    continue
                try:
                    self._write_chunk(chunk)
                except (BrokenPipeError, ConnectionResetError):
                    # The paid answer is still worth recording: keep reading
                    # upstream to the end and save it after the loop.
                    client_gone = True
                    self.close_connection = True
        except httpx.HTTPError as exc:
            print(
                f"proxy: upstream stream failed after the answer began: {exc}",
                file=sys.stderr,
            )
            self.close_connection = True
            return
        if not client_gone:
            try:
                self._end_chunks()
            except (BrokenPipeError, ConnectionResetError):
                self.close_connection = True
        if save:
            self._save_answer(
                key,
                role,
                path,
                request,
                response,
                stream=True,
                chunks=tuple(chunks),
            )

    def _save_answer(
        self,
        key: str,
        role: str,
        path: str,
        request: Any,
        response: httpx.Response,
        *,
        stream: bool,
        chunks: tuple[bytes, ...],
    ) -> None:
        self._save(
            Fixture(
                key=key,
                role=role,
                path=path,
                request=request,
                response=RecordedResponse(
                    status=response.status_code,
                    content_type=response.headers.get("Content-Type", "application/octet-stream"),
                    stream=stream,
                    chunks=chunks,
                ),
                recorded_at=_utc_now(),
            )
        )

    def _save(self, fixture: Fixture) -> None:
        try:
            self.server.store.save(fixture)
        except FixtureError as exc:
            # The answer already reached Trelis; losing the fixture is worth a
            # line on stderr, not a failed request whose model call was paid for.
            print(f"proxy: {exc}", file=sys.stderr)

    def _method_not_allowed(self) -> None:
        self._discard_body()
        self._send_json(
            405,
            _error(
                f"method {self.command} is not allowed; only POST is proxied",
                "method_not_allowed",
            ),
            headers={"Allow": "POST"},
        )

    def _discard_body(self) -> None:
        try:
            length = self._body_length()
        except _BadRequest:
            self.close_connection = True
            return
        if length == 0:
            return
        try:
            self.rfile.read(length)
        except OSError:
            self.close_connection = True

    def _send_json(
        self,
        status: int,
        payload: Any,
        *,
        headers: dict[str, str] | None = None,
    ) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if self.close_connection:
            self.send_header("Connection", "close")
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)
            self.wfile.flush()

    def _start_response(self, status: int, headers: list[tuple[str, str]]) -> None:
        """Start a relayed response with the upstream's own headers."""
        self.log_request(status)
        self.send_response_only(status)
        for name, value in headers:
            self.send_header(name, value)

    def _send_answer(self, status: int, headers: list[tuple[str, str]], payload: bytes) -> None:
        self._start_response(status, headers)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
        self.wfile.flush()

    def _write_chunk(self, chunk: bytes) -> None:
        if not chunk:
            return
        self.wfile.write(f"{len(chunk):X}\r\n".encode("ascii") + chunk + b"\r\n")
        self.wfile.flush()

    def _end_chunks(self) -> None:
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


def _relayed_headers(response: httpx.Response) -> list[tuple[str, str]]:
    """The upstream response headers the proxy copies verbatim."""
    relayed: list[tuple[str, str]] = []
    for name, value in response.headers.multi_items():
        lowered = name.lower()
        if lowered in _DROPPED_RESPONSE_HEADERS or lowered.startswith("proxy-"):
            continue
        relayed.append((name, value))
    return relayed


def _split_role(request_target: str) -> tuple[str, str]:
    """Split ``/role/rest`` into the role and the path after it.

    The path keeps the query string, if any, because it is forwarded to the
    provider and stored in the fixture; model endpoints carry their
    parameters in the body, so the usual path is just ``/embeddings``.
    """
    parsed = urlsplit(request_target)
    segments = parsed.path.split("/")
    role = segments[1] if len(segments) > 1 else ""
    path = "/" + "/".join(segments[2:])
    if parsed.query:
        path += "?" + parsed.query
    return role, path


def _is_stream(request: Any, content_type: str) -> bool:
    """Whether an answer is the sequence of chunks a fixture stores separately."""
    if isinstance(request, dict) and request.get("stream") is True:
        return True
    return content_type.split(";", 1)[0].strip().lower() == "text/event-stream"
