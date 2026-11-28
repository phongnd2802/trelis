"""An OpenAI-compatible Chat Completions client for a judge model.

The Grader that needs a model calls it from the harness, not through Trelis,
so the judge is configured separately from the answer generator. It speaks the
same Chat Completions shape the record/replay proxy serves, which is what lets
CI route the ``judge`` role through the proxy like any other role.
"""

from __future__ import annotations

import random
import time
from collections.abc import Callable
from typing import Any, Protocol

import httpx

MAX_ATTEMPTS = 3
"""How many times one judge call is sent in total."""

_BASE_DELAY = 0.5
_MAX_DELAY = 10.0
_MAX_RETRY_AFTER = 60.0


class Judge(Protocol):
    """What a Grader needs from a judge model: one prompt in, reply text out."""

    def complete(self, prompt: str) -> str:
        """Answer one prompt."""
        ...


class JudgeError(Exception):
    """A judge call that did not produce reply text.

    The message names what went wrong and may quote a short excerpt of the
    reply, but never the API key.
    """


class JudgeClient:
    """Calls one OpenAI-compatible chat model with a single prompt."""

    def __init__(
        self,
        base_url: str,
        api_key: str,
        model: str,
        *,
        client: httpx.Client | None = None,
        transport: httpx.BaseTransport | None = None,
        timeout: float = 60.0,
        max_attempts: int = MAX_ATTEMPTS,
        sleep: Callable[[float], None] = time.sleep,
    ):
        if not api_key:
            raise ValueError("a judge API key is required")
        if not model:
            raise ValueError("a judge model is required")
        if not isinstance(max_attempts, int) or isinstance(max_attempts, bool) or max_attempts < 1:
            raise ValueError(f"a judge needs at least one attempt, got {max_attempts!r}")
        if client is not None and transport is not None:
            raise ValueError("pass either an httpx.Client or a transport, not both")
        self._base_url = base_url.rstrip("/")
        self._model = model
        self._max_attempts = max_attempts
        self._sleep = sleep
        self._headers = {"Authorization": f"Bearer {api_key}"}
        self._owns_client = client is None
        self._client = client or httpx.Client(transport=transport, timeout=timeout)

    def __enter__(self) -> JudgeClient:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def close(self) -> None:
        """Close the underlying HTTP client when this object created it."""
        if self._owns_client:
            self._client.close()

    def complete(self, prompt: str) -> str:
        """Send one user message and return the assistant's text.

        A 429, a 5xx or a transport failure is retried with backoff, honouring
        ``Retry-After`` up to a cap; another 4xx or a 2xx whose reply cannot be
        read is returned at once. The last failure is raised: a non-2xx answer
        as :class:`JudgeError`, a transport failure as ``httpx.HTTPError``.
        """
        last_error: Exception | None = None
        for attempt in range(1, self._max_attempts + 1):
            try:
                response = self._client.post(
                    f"{self._base_url}/chat/completions",
                    headers=self._headers,
                    json={
                        "model": self._model,
                        "messages": [{"role": "user", "content": prompt}],
                        "temperature": 0,
                    },
                )
            except httpx.TransportError as exc:
                last_error = exc
                if attempt == self._max_attempts:
                    break
                self._sleep(_backoff(attempt))
                continue

            if response.is_success:
                try:
                    body = response.json()
                except ValueError:
                    raise JudgeError(
                        f"judge reply is not JSON: {_excerpt(response.text)}"
                    ) from None
                content = _reply_content(body)
                if content is None:
                    raise JudgeError(
                        "judge reply has no choices[0].message.content: " + _excerpt(response.text)
                    )
                return content

            error = JudgeError(
                f"judge answered HTTP {response.status_code}: {_error_message(response)}"
            )
            if not _retryable(response.status_code) or attempt == self._max_attempts:
                raise error
            wait = _retry_wait(attempt, response)
            if wait is None:
                raise error
            last_error = error
            self._sleep(wait)

        assert last_error is not None
        raise last_error


def _retryable(status_code: int) -> bool:
    """Say whether a status is worth sending again: 429 or a 5xx."""
    return status_code == httpx.codes.TOO_MANY_REQUESTS or 500 <= status_code <= 599


def _retry_wait(attempt: int, response: httpx.Response) -> float | None:
    """Return the wait before the next attempt, or None to stop retrying.

    A provider's ``Retry-After`` replaces the exponential backoff, may only be
    lengthened by jitter, and stops the retries when it is longer than the cap.
    """
    after = _retry_after(response)
    if after is not None:
        if after > _MAX_RETRY_AFTER:
            return None
        return min(after + _jitter(after), _MAX_RETRY_AFTER)
    delay = _backoff(attempt)
    return delay - _jitter(delay)


def _retry_after(response: httpx.Response) -> float | None:
    """Read ``Retry-After`` in seconds from a 429 or 503, or None."""
    if response.status_code not in (
        httpx.codes.TOO_MANY_REQUESTS,
        httpx.codes.SERVICE_UNAVAILABLE,
    ):
        return None
    value = response.headers.get("Retry-After")
    if value is None:
        return None
    try:
        seconds = float(value)
    except ValueError:
        return None
    return seconds if seconds >= 0 else None


def _backoff(attempt: int) -> float:
    """Return the exponential backoff after the given failed attempt."""
    return min(_BASE_DELAY * 2 ** (attempt - 1), _MAX_DELAY)


def _jitter(seconds: float) -> float:
    """Return up to a quarter of ``seconds``, so callers do not retry in lockstep."""
    return random.uniform(0.0, seconds / 4) if seconds > 0 else 0.0


def _reply_content(body: Any) -> str | None:
    """Return the first choice's message text, or None when the shape is wrong."""
    if not isinstance(body, dict):
        return None
    choices = body.get("choices")
    if not isinstance(choices, list) or not choices:
        return None
    first = choices[0]
    if not isinstance(first, dict):
        return None
    message = first.get("message")
    if not isinstance(message, dict):
        return None
    content = message.get("content")
    return content if isinstance(content, str) else None


def _error_message(response: httpx.Response) -> str:
    """Read a provider error message, falling back to a short body excerpt."""
    try:
        body = response.json()
    except ValueError:
        return _excerpt(response.text)
    if isinstance(body, dict):
        error = body.get("error")
        if isinstance(error, dict) and isinstance(error.get("message"), str):
            return error["message"]
        if isinstance(error, str):
            return error
        if isinstance(body.get("message"), str):
            return body["message"]
    return _excerpt(response.text)


def _excerpt(text: str, limit: int = 200) -> str:
    """Cut text to about ``limit`` characters for a rationale or error."""
    text = text.strip()
    return text if len(text) <= limit else text[:limit] + "..."
