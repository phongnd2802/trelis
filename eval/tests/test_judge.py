"""Tests for the OpenAI-compatible judge client, using MockTransport."""

import json

import httpx
import pytest

from trelis_eval.judge import JudgeClient, JudgeError

BASE_URL = "http://judge.test/v1"
API_KEY = "judge-key-never-logged"
MODEL = "judge-model"


def make_judge(handler, *, sleeps=None, max_attempts=3):
    def sleep(seconds):
        if sleeps is not None:
            sleeps.append(seconds)

    return JudgeClient(
        BASE_URL,
        API_KEY,
        MODEL,
        transport=httpx.MockTransport(handler),
        max_attempts=max_attempts,
        sleep=sleep,
    )


def chat_reply(content):
    return {"choices": [{"index": 0, "message": {"role": "assistant", "content": content}}]}


def test_complete_posts_chat_completions_with_temperature_zero_and_the_bearer_key():
    seen = {}

    def handler(request):
        seen["method"] = request.method
        seen["url"] = str(request.url)
        seen["authorization"] = request.headers["authorization"]
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json=chat_reply("same answer"))

    with make_judge(handler) as judge:
        reply = judge.complete("Compare these answers.")

    assert reply == "same answer"
    assert seen == {
        "method": "POST",
        "url": f"{BASE_URL}/chat/completions",
        "authorization": f"Bearer {API_KEY}",
        "body": {
            "model": MODEL,
            "messages": [{"role": "user", "content": "Compare these answers."}],
            "temperature": 0,
        },
    }


def test_a_429_is_retried_then_succeeds():
    calls = []
    sleeps = []

    def handler(request):
        calls.append(request)
        if len(calls) == 1:
            return httpx.Response(429, json={"error": {"message": "slow down"}})
        return httpx.Response(200, json=chat_reply("same answer"))

    with make_judge(handler, sleeps=sleeps) as judge:
        reply = judge.complete("Compare.")

    assert reply == "same answer"
    assert len(calls) == 2
    assert len(sleeps) == 1


def test_500_three_times_is_a_judge_error_after_three_attempts():
    calls = []
    sleeps = []

    def handler(request):
        calls.append(request)
        return httpx.Response(500, json={"error": {"message": "judge is down"}})

    with make_judge(handler, sleeps=sleeps) as judge, pytest.raises(JudgeError) as caught:
        judge.complete("Compare.")

    assert "HTTP 500" in str(caught.value)
    assert API_KEY not in str(caught.value)
    assert len(calls) == 3
    assert len(sleeps) == 2


def test_a_400_is_not_retried():
    calls = []
    sleeps = []

    def handler(request):
        calls.append(request)
        return httpx.Response(400, json={"error": {"message": "bad request"}})

    with make_judge(handler, sleeps=sleeps) as judge, pytest.raises(JudgeError) as caught:
        judge.complete("Compare.")

    assert "HTTP 400" in str(caught.value)
    assert len(calls) == 1
    assert sleeps == []


def test_a_timeout_is_retried_then_succeeds():
    calls = []
    sleeps = []

    def handler(request):
        calls.append(request)
        if len(calls) == 1:
            raise httpx.ReadTimeout("judge timed out", request=request)
        return httpx.Response(200, json=chat_reply("same answer"))

    with make_judge(handler, sleeps=sleeps) as judge:
        reply = judge.complete("Compare.")

    assert reply == "same answer"
    assert len(calls) == 2
    assert len(sleeps) == 1


def test_a_timeout_that_keeps_failing_surfaces_as_an_httpx_error():
    def handler(request):
        raise httpx.ReadTimeout("judge timed out", request=request)

    with make_judge(handler) as judge, pytest.raises(httpx.HTTPError):
        judge.complete("Compare.")


def test_retry_after_is_honoured_with_jitter_up_to_the_cap():
    calls = []
    sleeps = []

    def handler(request):
        calls.append(request)
        if len(calls) == 1:
            return httpx.Response(
                429,
                headers={"Retry-After": "2"},
                json={"error": {"message": "slow down"}},
            )
        return httpx.Response(200, json=chat_reply("same answer"))

    with make_judge(handler, sleeps=sleeps) as judge:
        judge.complete("Compare.")

    assert len(calls) == 2
    assert len(sleeps) == 1
    assert 2.0 <= sleeps[0] <= 2.5


def test_a_retry_after_beyond_the_cap_is_not_waited_out():
    calls = []
    sleeps = []

    def handler(request):
        calls.append(request)
        return httpx.Response(
            429,
            headers={"Retry-After": "61"},
            json={"error": {"message": "come back later"}},
        )

    with make_judge(handler, sleeps=sleeps) as judge, pytest.raises(JudgeError):
        judge.complete("Compare.")

    assert len(calls) == 1
    assert sleeps == []


def test_a_reply_without_a_message_content_is_not_retried():
    calls = []

    def handler(request):
        calls.append(request)
        return httpx.Response(200, json={"choices": []})

    with make_judge(handler) as judge, pytest.raises(JudgeError) as caught:
        judge.complete("Compare.")

    message = str(caught.value)
    assert len(calls) == 1
    assert "choices" in message
    assert '"choices":[]' in message
    assert len(message) < 400


def test_a_non_json_success_body_is_a_judge_error_with_the_reply_text():
    def handler(request):
        return httpx.Response(200, text="<html>not json</html>")

    with make_judge(handler) as judge, pytest.raises(JudgeError) as caught:
        judge.complete("Compare.")

    assert "not JSON" in str(caught.value)
    assert "<html>not json</html>" in str(caught.value)


def test_a_judge_requires_a_key_a_model_and_at_least_one_attempt():
    with pytest.raises(ValueError, match="API key"):
        JudgeClient(BASE_URL, "", MODEL)
    with pytest.raises(ValueError, match="model"):
        JudgeClient(BASE_URL, API_KEY, "")
    with pytest.raises(ValueError, match="attempt"):
        JudgeClient(BASE_URL, API_KEY, MODEL, max_attempts=0)
