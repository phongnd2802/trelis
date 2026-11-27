"""Tests for the record/replay model proxy, against a fake upstream server."""

import base64
import errno
import gzip
import http.client
import json
import socket
import struct
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import httpx
import pytest

from trelis_eval.model_proxy import (
    Fixture,
    FixtureStore,
    ProxyConfig,
    ProxyMode,
    RecordedResponse,
    base_url_lines,
    fixture_key,
    normalize_body,
    parse_listen,
    parse_upstreams,
    serve,
)

API_KEY = "sk-test-secret-never-stored"
AUTHORIZATION = f"Bearer {API_KEY}"

EMBEDDING_BODY = (
    b'{"object": "list", "data": [{"embedding": [0.1, 0.2], "index": 0}], '
    b'"usage": {"prompt_tokens": 7, "total_tokens": 7}}'
)
STREAM_CHUNKS = (
    b'data: {"choices": [{"delta": {"content": "Hel"}}]}\n\n',
    b'data: {"choices": [{"delta": {"content": "lo"}}]}\n\n',
    b"data: [DONE]\n\n",
)
FAILURE_BODY = b'{"error": {"message": "provider exploded"}}'


class FakeUpstream(ThreadingHTTPServer):
    """A fake OpenAI-compatible provider on port 0, recording what it saw."""

    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), FakeUpstreamHandler)
        self.requests = []
        self.first_chunk_sent = threading.Event()
        self.release_stream = threading.Event()
        self.pause_between_chunks = False
        self.release_chunk = threading.Semaphore(0)
        # By default the stream never pauses; the streaming tests change this.
        self.release_stream.set()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server_address[1]}"


class FakeUpstreamHandler(BaseHTTPRequestHandler):
    """Serves an embeddings answer and an SSE stream from the fake upstream."""

    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        self.server.requests.append(
            {
                "path": self.path,
                "authorization": self.headers.get("Authorization"),
                "content_type": self.headers.get("Content-Type"),
                "accept": self.headers.get("Accept"),
                "body": body,
            }
        )
        if b"upstream-failure" in body:
            self._send(500, "application/json", FAILURE_BODY)
        elif b"rate-limited" in body:
            self._send(
                429,
                "application/json",
                FAILURE_BODY,
                extra_headers={"Retry-After": "7", "x-ratelimit-remaining": "0"},
            )
        elif b"gzip-response" in body:
            self._send(
                200,
                "application/json",
                gzip.compress(EMBEDDING_BODY),
                extra_headers={"Content-Encoding": "gzip"},
            )
        elif self.path.endswith("/embeddings"):
            self._send(
                200,
                "application/json",
                EMBEDDING_BODY,
                extra_headers={"x-request-id": "req-1"},
            )
        elif self.path.endswith("/chat/completions"):
            self._send_stream()
        else:
            self._send(404, "application/json", b'{"error": "no such path"}')

    def _send(self, status, content_type, body, extra_headers=None):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        for name, value in (extra_headers or {}).items():
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()

    def _send_stream(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for index, chunk in enumerate(STREAM_CHUNKS):
            self._write_chunk(chunk)
            if self.server.pause_between_chunks:
                if not self.server.release_chunk.acquire(timeout=10):
                    raise RuntimeError("the test never released a chunk")
            elif index == 0:
                self.server.first_chunk_sent.set()
                if not self.server.release_stream.wait(timeout=10):
                    raise RuntimeError("the test never released the stream")
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

    def _write_chunk(self, data):
        self.wfile.write(f"{len(data):X}\r\n".encode("ascii") + data + b"\r\n")
        self.wfile.flush()


class RunningProxy:
    """A proxy server serving on port 0 in a background thread."""

    def __init__(self, config):
        self.server = serve(config, "127.0.0.1", 0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base_url = f"http://127.0.0.1:{self.server.server_address[1]}"

    def __enter__(self):
        return self

    def __exit__(self, *exc_info):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=10)


@pytest.fixture
def upstream():
    server = FakeUpstream()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield server
    server.shutdown()
    server.server_close()
    thread.join(timeout=10)


def post(base_url, role, path, body, **headers):
    return httpx.post(
        f"{base_url}/{role}{path}",
        content=body,
        headers={
            "Authorization": AUTHORIZATION,
            "Content-Type": "application/json",
            **headers,
        },
        timeout=15,
    )


def fixture_files(fixtures_dir: Path):
    if not fixtures_dir.exists():
        return []
    return [path for path in fixtures_dir.rglob("*") if path.is_file()]


def test_normalize_body_sorts_keys_and_drops_insignificant_whitespace():
    original = b'{\n  "model": "m",\n  "input": ["a", "b"]\n}'
    reordered = b'{"input":["a","b"],"model":"m"}'

    assert normalize_body(original) == b'{"input":["a","b"],"model":"m"}'
    assert normalize_body(reordered) == normalize_body(original)


def test_normalize_body_keeps_non_ascii_and_leaves_non_json_alone():
    assert normalize_body('{"text": "xin chào"}'.encode()) == '{"text":"xin chào"}'.encode()
    assert normalize_body(b"not json \xff") == b"not json \xff"


def test_fixture_key_ignores_key_order_and_whitespace_but_not_values():
    first = b'{"model": "m", "input": ["a", "b"]}'
    second = b'{"input": ["a", "b"], "model": "m"}'

    key = fixture_key("embedding", "/embeddings", first)

    assert fixture_key("embedding", "/embeddings", second) == key
    assert fixture_key("embedding", "/embeddings", b'{"model": "m", "input": ["a"]}') != key
    assert fixture_key("chat", "/embeddings", first) != key
    assert fixture_key("embedding", "/chat/completions", first) != key


def test_fixture_key_hashes_a_non_json_body_as_raw_bytes():
    assert fixture_key("embedding", "/embeddings", b"\xff\x00") == fixture_key(
        "embedding", "/embeddings", b"\xff\x00"
    )
    assert fixture_key("embedding", "/embeddings", b"\xff\x00") != fixture_key(
        "embedding", "/embeddings", b"\xff "
    )


def make_fixture(key="ab" + "0" * 62, chunks=(b"\xff\x00\xfe", b"tail")):
    return Fixture(
        key=key,
        role="embedding",
        path="/embeddings",
        request={"model": "m", "input": ["a"]},
        response=RecordedResponse(
            status=200,
            content_type="application/json",
            stream=False,
            chunks=chunks,
        ),
        recorded_at="2026-01-01T00:00:00Z",
    )


def test_fixture_store_round_trips_exact_bytes_including_non_utf8(tmp_path):
    store = FixtureStore(tmp_path / "fixtures")
    fixture = make_fixture()

    store.save(fixture)
    loaded = store.load("embedding", fixture.key)

    assert loaded == fixture
    assert loaded.response.chunks == (b"\xff\x00\xfe", b"tail")
    assert list((tmp_path / "fixtures").rglob("*.tmp")) == []


def test_fixture_store_load_returns_none_for_a_missing_fixture(tmp_path):
    store = FixtureStore(tmp_path / "fixtures")

    assert store.load("embedding", "0" * 64) is None


def test_fixture_store_records_each_miss_as_one_json_line(tmp_path):
    store = FixtureStore(tmp_path / "fixtures")

    store.record_miss("embedding", "/embeddings", "a" * 64, {"model": "m", "input": ["a"]})
    store.record_miss("chat", "/chat/completions", "b" * 64, {"model": "chat"})

    lines = (tmp_path / "fixtures" / "misses.jsonl").read_text(encoding="utf-8").splitlines()
    assert [json.loads(line)["key"] for line in lines] == ["a" * 64, "b" * 64]
    assert json.loads(lines[0])["request"] == {"model": "m", "input": ["a"]}
    assert json.loads(lines[1])["role"] == "chat"


def test_record_forwards_the_request_and_saves_a_reusable_fixture(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    body = b'{"model": "embedding-model", "input": ["alpha"]}'
    reordered = b'{"input":["alpha"],"model":"embedding-model"}'
    key = fixture_key("embedding", "/embeddings", body)
    config = ProxyConfig(ProxyMode.RECORD, fixtures, {"embedding": upstream.url})

    with RunningProxy(config) as proxy:
        first = post(proxy.base_url, "embedding", "/embeddings", body, Accept="application/json")
        second = post(proxy.base_url, "embedding", "/embeddings", reordered)

    assert first.status_code == 200
    assert first.content == EMBEDDING_BODY
    assert first.headers["content-type"] == "application/json"
    assert first.headers["x-request-id"] == "req-1"
    assert second.content == EMBEDDING_BODY
    assert "x-request-id" not in second.headers
    assert len(upstream.requests) == 1
    seen = upstream.requests[0]
    assert seen["path"] == "/embeddings"
    assert seen["authorization"] == AUTHORIZATION
    assert seen["content_type"] == "application/json"
    assert seen["accept"] == "application/json"
    assert seen["body"] == body

    saved = json.loads((fixtures / "embedding" / key[:2] / f"{key}.json").read_text())
    assert saved["schema_version"] == 1
    assert saved["key"] == key
    assert saved["role"] == "embedding"
    assert saved["path"] == "/embeddings"
    assert saved["request"] == {"model": "embedding-model", "input": ["alpha"]}
    assert saved["response"]["status"] == 200
    assert saved["response"]["stream"] is False
    assert saved["response"]["chunks_b64"] == [base64.b64encode(EMBEDDING_BODY).decode("ascii")]


def test_record_passes_a_non_2xx_answer_back_and_saves_nothing(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    config = ProxyConfig(ProxyMode.RECORD, fixtures, {"embedding": upstream.url})
    body = b'{"model": "upstream-failure", "input": ["alpha"]}'

    with RunningProxy(config) as proxy:
        response = post(proxy.base_url, "embedding", "/embeddings", body)

    assert response.status_code == 500
    assert response.content == FAILURE_BODY
    assert fixture_files(fixtures) == []


@pytest.mark.parametrize("mode", [ProxyMode.RECORD, ProxyMode.PASSTHROUGH])
def test_upstream_error_headers_are_relayed_in_record_and_passthrough(tmp_path, upstream, mode):
    fixtures = tmp_path / "fixtures"
    config = ProxyConfig(mode, fixtures, {"embedding": upstream.url})
    body = b'{"model": "rate-limited", "input": ["alpha"]}'

    with RunningProxy(config) as proxy:
        response = post(proxy.base_url, "embedding", "/embeddings", body)

    assert response.status_code == 429
    assert response.headers["retry-after"] == "7"
    assert response.headers["x-ratelimit-remaining"] == "0"
    assert response.content == FAILURE_BODY
    assert fixture_files(fixtures) == []


def test_replay_serves_the_recorded_bytes_without_calling_an_upstream(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    body = b'{"model": "embedding-model", "input": ["alpha"]}'
    record = ProxyConfig(ProxyMode.RECORD, fixtures, {"embedding": upstream.url})
    with RunningProxy(record) as proxy:
        recorded = post(proxy.base_url, "embedding", "/embeddings", body)

    replay = ProxyConfig(ProxyMode.REPLAY, fixtures, {})
    with RunningProxy(replay) as proxy:
        replayed = post(proxy.base_url, "embedding", "/embeddings", body)

    assert replayed.status_code == recorded.status_code
    assert replayed.content == recorded.content == EMBEDDING_BODY
    assert replayed.headers["content-type"] == recorded.headers["content-type"]
    assert len(upstream.requests) == 1
    assert json.loads(replayed.content)["usage"] == {
        "prompt_tokens": 7,
        "total_tokens": 7,
    }


def test_compressed_upstream_bodies_are_decoded_and_replayed_plain(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    body = b'{"model": "gzip-response", "input": ["alpha"]}'
    key = fixture_key("embedding", "/embeddings", body)
    record = ProxyConfig(ProxyMode.RECORD, fixtures, {"embedding": upstream.url})

    with RunningProxy(record) as proxy:
        recorded = post(proxy.base_url, "embedding", "/embeddings", body)

    assert recorded.status_code == 200
    assert recorded.content == EMBEDDING_BODY
    assert "content-encoding" not in recorded.headers

    fixture = FixtureStore(fixtures).load("embedding", key)
    assert fixture is not None
    assert b"".join(fixture.response.chunks) == EMBEDDING_BODY

    replay = ProxyConfig(ProxyMode.REPLAY, fixtures, {})
    with RunningProxy(replay) as proxy:
        replayed = post(proxy.base_url, "embedding", "/embeddings", body)

    assert replayed.content == EMBEDDING_BODY
    assert "content-encoding" not in replayed.headers


def test_record_streams_chunks_as_they_arrive_and_replay_keeps_them_in_order(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    body = json.dumps(
        {"model": "chat-model", "stream": True, "messages": [{"role": "user", "content": "Hi"}]}
    ).encode()
    expected = b"".join(STREAM_CHUNKS)
    upstream.release_stream.clear()
    config = ProxyConfig(ProxyMode.RECORD, fixtures, {"chat": upstream.url})

    with RunningProxy(config) as proxy:
        with httpx.Client(timeout=15) as client:
            with client.stream(
                "POST",
                f"{proxy.base_url}/chat/chat/completions",
                content=body,
                headers={
                    "Authorization": AUTHORIZATION,
                    "Content-Type": "application/json",
                    "Accept": "text/event-stream",
                },
            ) as response:
                assert response.status_code == 200
                assert response.headers["content-type"] == "text/event-stream"
                raw = response.iter_raw()
                first = next(raw)
                assert first, "the proxy sent the first chunk only after the stream ended"
                assert expected.startswith(first)
                upstream.release_stream.set()
                recorded = first + b"".join(raw)

    assert recorded == expected
    saved = list(fixtures.rglob("*.json"))
    assert len(saved) == 1
    document = json.loads(saved[0].read_text())
    assert document["response"]["stream"] is True
    stored = b"".join(base64.b64decode(chunk) for chunk in document["response"]["chunks_b64"])
    assert stored == expected

    replay = ProxyConfig(ProxyMode.REPLAY, fixtures, {})
    with RunningProxy(replay) as proxy:
        with httpx.Client(timeout=15) as client:
            with client.stream(
                "POST",
                f"{proxy.base_url}/chat/chat/completions",
                content=body,
                headers={
                    "Authorization": AUTHORIZATION,
                    "Content-Type": "application/json",
                    "Accept": "text/event-stream",
                },
            ) as response:
                assert response.status_code == 200
                assert response.headers["content-type"] == "text/event-stream"
                replayed = b"".join(response.iter_raw())

    assert replayed == expected
    assert len(upstream.requests) == 1


def test_record_stores_one_chunk_per_upstream_write_in_order(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    body = json.dumps(
        {"model": "chat-model", "stream": True, "messages": [{"role": "user", "content": "Hi"}]}
    ).encode()
    key = fixture_key("chat", "/chat/completions", body)
    upstream.pause_between_chunks = True
    config = ProxyConfig(ProxyMode.RECORD, fixtures, {"chat": upstream.url})

    with RunningProxy(config) as proxy:
        with httpx.Client(timeout=15) as client:
            with client.stream(
                "POST",
                f"{proxy.base_url}/chat/chat/completions",
                content=body,
                headers={
                    "Authorization": AUTHORIZATION,
                    "Content-Type": "application/json",
                    "Accept": "text/event-stream",
                },
            ) as response:
                assert response.status_code == 200
                pieces = response.iter_bytes()
                received = []
                for expected in STREAM_CHUNKS:
                    piece = next(pieces)
                    assert piece == expected
                    received.append(piece)
                    upstream.release_chunk.release()
                assert next(pieces, None) is None

    assert b"".join(received) == b"".join(STREAM_CHUNKS)
    fixture = FixtureStore(fixtures).load("chat", key)
    assert fixture is not None
    assert fixture.response.chunks == STREAM_CHUNKS

    replay = ProxyConfig(ProxyMode.REPLAY, fixtures, {})
    with RunningProxy(replay) as proxy:
        with httpx.Client(timeout=15) as client:
            with client.stream(
                "POST",
                f"{proxy.base_url}/chat/chat/completions",
                content=body,
                headers={
                    "Authorization": AUTHORIZATION,
                    "Content-Type": "application/json",
                },
            ) as response:
                assert b"".join(response.iter_bytes()) == b"".join(STREAM_CHUNKS)


def test_record_keeps_draining_and_saves_when_the_client_disconnects(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    body = json.dumps(
        {"model": "chat-model", "stream": True, "messages": [{"role": "user", "content": "Hi"}]}
    ).encode()
    key = fixture_key("chat", "/chat/completions", body)
    upstream.release_stream.clear()
    config = ProxyConfig(ProxyMode.RECORD, fixtures, {"chat": upstream.url})

    with RunningProxy(config) as proxy:
        host, port = proxy.server.server_address[:2]
        sock = socket.create_connection((host, port), timeout=10)
        request = (
            b"POST /chat/chat/completions HTTP/1.1\r\n"
            b"Host: proxy\r\n"
            b"Authorization: " + AUTHORIZATION.encode() + b"\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: " + str(len(body)).encode() + b"\r\n"
            b"Connection: close\r\n\r\n" + body
        )
        sock.sendall(request)
        received = b""
        while STREAM_CHUNKS[0] not in received:
            data = sock.recv(65536)
            assert data, "the proxy closed the stream before the first chunk"
            received += data
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
        sock.close()
        time.sleep(0.05)
        upstream.release_stream.set()

        path = fixtures / "chat" / key[:2] / f"{key}.json"
        deadline = time.monotonic() + 10
        while not path.exists():
            assert time.monotonic() < deadline, "the proxy did not save the drained stream"
            time.sleep(0.01)

    fixture = FixtureStore(fixtures).load("chat", key)
    assert fixture is not None
    assert b"".join(fixture.response.chunks) == b"".join(STREAM_CHUNKS)


def test_replay_miss_returns_404_names_the_request_and_appends_to_the_miss_log(tmp_path, capsys):
    fixtures = tmp_path / "fixtures"
    body = b'{"model": "missing-model", "input": ["alpha"]}'
    key = fixture_key("embedding", "/embeddings", body)
    config = ProxyConfig(ProxyMode.REPLAY, fixtures, {})

    with RunningProxy(config) as proxy:
        response = post(proxy.base_url, "embedding", "/embeddings", body)

    assert response.status_code == 404
    error = response.json()["error"]
    assert error["type"] == "replay_miss"
    assert error["key"] == key
    assert "embedding" in error["message"]
    assert "/embeddings" in error["message"]
    assert key in error["message"]
    assert "re-record with --mode record" in error["message"]

    stderr = capsys.readouterr().err
    assert "replay miss" in stderr
    assert "embedding" in stderr
    assert "/embeddings" in stderr
    assert key in stderr
    assert "missing-model" in stderr
    assert API_KEY not in stderr

    lines = (fixtures / "misses.jsonl").read_text(encoding="utf-8").splitlines()
    assert API_KEY not in "\n".join(lines)
    assert len(lines) == 1
    miss = json.loads(lines[0])
    assert miss["key"] == key
    assert miss["role"] == "embedding"
    assert miss["path"] == "/embeddings"
    assert miss["request"] == {"model": "missing-model", "input": ["alpha"]}
    assert fixture_files(fixtures) == [fixtures / "misses.jsonl"]


def test_passthrough_forwards_and_saves_nothing(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    body = b'{"model": "embedding-model", "input": ["alpha"]}'
    config = ProxyConfig(ProxyMode.PASSTHROUGH, fixtures, {"embedding": upstream.url})

    with RunningProxy(config) as proxy:
        response = post(proxy.base_url, "embedding", "/embeddings", body)

    assert response.content == EMBEDDING_BODY
    assert response.headers["x-request-id"] == "req-1"
    assert len(upstream.requests) == 1
    assert fixture_files(fixtures) == []


def test_unknown_role_is_404_and_other_methods_are_405(tmp_path, upstream):
    config = ProxyConfig(ProxyMode.RECORD, tmp_path / "fixtures", {"embedding": upstream.url})

    with RunningProxy(config) as proxy:
        unknown = post(proxy.base_url, "unknown", "/embeddings", b"{}")
        get = httpx.get(f"{proxy.base_url}/embedding/embeddings", timeout=15)

    assert unknown.status_code == 404
    message = unknown.json()["error"]["message"]
    assert "unknown" in message
    assert "embedding" in message
    assert get.status_code == 405
    assert get.headers["allow"] == "POST"
    assert upstream.requests == []


def test_a_rejected_chunked_request_closes_the_connection_cleanly(tmp_path, upstream):
    config = ProxyConfig(ProxyMode.RECORD, tmp_path / "fixtures", {"embedding": upstream.url})
    body = b'{"model": "embedding-model", "input": ["alpha"]}'

    with RunningProxy(config) as proxy:
        host, port = proxy.server.server_address[:2]
        connection = http.client.HTTPConnection(host, port, timeout=10)
        connection.request(
            "POST",
            "/embedding/embeddings",
            body=iter([body]),
            headers={
                "Authorization": AUTHORIZATION,
                "Content-Type": "application/json",
                "Transfer-Encoding": "chunked",
            },
            encode_chunked=True,
        )
        rejected = connection.getresponse()
        assert rejected.status == 400
        assert "chunked request bodies" in rejected.read().decode()

        connection.request(
            "POST",
            "/embedding/embeddings",
            body=body,
            headers={
                "Authorization": AUTHORIZATION,
                "Content-Type": "application/json",
                "Content-Length": str(len(body)),
            },
        )
        accepted = connection.getresponse()
        assert accepted.status == 200
        assert accepted.read() == EMBEDDING_BODY
        connection.close()


def test_no_header_name_or_api_key_is_written_under_the_fixtures_directory(tmp_path, upstream):
    fixtures = tmp_path / "fixtures"
    config = ProxyConfig(ProxyMode.RECORD, fixtures, {"embedding": upstream.url})
    body = b'{"model": "embedding-model", "input": ["alpha"]}'

    with RunningProxy(config) as proxy:
        post(proxy.base_url, "embedding", "/embeddings", body)

    files = fixture_files(fixtures)
    assert len(files) == 1
    text = files[0].read_text(encoding="utf-8")
    assert API_KEY not in text
    assert "authorization" not in text.lower()
    assert "bearer" not in text.lower()


def test_parse_upstreams_accepts_role_url_pairs():
    upstreams = parse_upstreams(
        ["embedding=https://openrouter.ai/api/v1", "chat=http://127.0.0.1:9000/v1"]
    )

    assert upstreams == {
        "embedding": "https://openrouter.ai/api/v1",
        "chat": "http://127.0.0.1:9000/v1",
    }


@pytest.mark.parametrize(
    "value",
    [
        "embedding",
        "=https://openrouter.ai/api/v1",
        "embedding=",
        "Embedding=https://openrouter.ai/api/v1",
        "emb3dding=https://openrouter.ai/api/v1",
        "embedding=ftp://openrouter.ai/api/v1",
        "embedding=https://",
        "embedding=https://openrouter.ai/api/v1?key=secret",
    ],
)
def test_parse_upstreams_rejects_a_malformed_role_or_url(value):
    with pytest.raises(ValueError, match="--upstream"):
        parse_upstreams([value])


def test_parse_upstreams_rejects_a_role_named_twice():
    with pytest.raises(ValueError, match="more than once"):
        parse_upstreams(["embedding=https://one.test/v1", "embedding=https://two.test/v1"])


def test_parse_upstreams_rejects_credentials_in_the_url():
    with pytest.raises(ValueError, match="TRELIS_MODELS_EMBEDDING_API_KEY"):
        parse_upstreams(["embedding=https://user:secret@openrouter.ai/api/v1"])


def test_parse_listen_splits_host_and_port():
    assert parse_listen("127.0.0.1:4100") == ("127.0.0.1", 4100)
    assert parse_listen("[::1]:4100") == ("::1", 4100)


@pytest.mark.parametrize("value", ["4100", ":4100", "host:port", "host:70000", "host:-1"])
def test_parse_listen_rejects_a_malformed_address(value):
    with pytest.raises(ValueError, match="--listen"):
        parse_listen(value)


def test_base_url_lines_names_the_role_env_variables_in_order():
    lines = base_url_lines("127.0.0.1", 4100, ["embedding", "chat"])

    assert lines == [
        "TRELIS_MODELS_CHAT_BASE_URL=http://127.0.0.1:4100/chat",
        "TRELIS_MODELS_EMBEDDING_BASE_URL=http://127.0.0.1:4100/embedding",
    ]


def test_serve_surfaces_a_taken_port_as_os_error(tmp_path):
    config = ProxyConfig(ProxyMode.REPLAY, tmp_path / "fixtures")
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as holder:
        holder.bind(("127.0.0.1", 0))
        holder.listen()
        port = holder.getsockname()[1]

        with pytest.raises(Exception) as caught:
            serve(config, "127.0.0.1", port)

    assert isinstance(caught.value, OSError)
    assert caught.value.errno == errno.EADDRINUSE
