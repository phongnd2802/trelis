"""Shared fakes for the eval harness tests, so two test modules use one script."""

import json

import httpx

USAGE = {"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}


class ScriptedJudge:
    """A judge that returns prepared replies, or raises a prepared error."""

    def __init__(self, *replies: str, error: Exception | None = None):
        self.replies = list(replies)
        self.error = error
        self.prompts: list[str] = []

    def complete(self, prompt: str) -> str:
        """Record the prompt and answer with the next scripted reply."""
        self.prompts.append(prompt)
        if self.error is not None:
            raise self.error
        if not self.replies:
            raise AssertionError("the judge was asked more times than the test scripted")
        return self.replies.pop(0)


class AnswerServer:
    """A MockTransport handler for POST /answer, driven by query-to-spec maps."""

    def __init__(self, answers_by_query, *, failing_queries=(), failure_status=400):
        self.answers_by_query = answers_by_query
        self.failing_queries = set(failing_queries)
        self.failure_status = failure_status
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
                    "error": {"code": code, "message": "answer failed", "details": None},
                },
            )
        spec = self.answers_by_query.get(body["query"], {})
        return httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "answer": spec.get("answer", "An answer."),
                    "citations": spec.get("citations", []),
                    "finish_reason": "stop",
                    "debug": {
                        "results": spec.get("results", []),
                        "trace": spec.get("trace", []),
                        "usage": USAGE,
                    },
                },
            },
        )
