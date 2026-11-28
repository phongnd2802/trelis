"""Graders score one answer on one aspect, and say when they could not.

A Grader never raises for a bad reply or a judge failure: it returns a
``Grade`` with status ``grader_error``, which an Ablation Run excludes from the
mean and counts instead of scoring as zero. The registry maps a name to a
factory, so a later Grader (faithfulness, citation precision) needs no new
plumbing in the run or the report.
"""

from __future__ import annotations

import json
import re
import unicodedata
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from enum import StrEnum
from typing import Any, Protocol

import httpx

from .client import Citation, RetrievedChunk
from .judge import Judge, JudgeError, _excerpt

CORRECTNESS = "correctness"

_SOURCE_MARKER = re.compile(r"\[\s*\d+\s*\]")

_CORRECTNESS_PROMPT = (
    "You check whether a candidate answer says the same thing as the expected answer.\n"
    "Question: {question}\n"
    "Expected answer: {expected_answer}\n"
    "Candidate answer: {answer}\n"
    "Reply with JSON only, no prose and no code fence, in this shape: "
    '{{"correct": true, "rationale": "one short sentence"}}; '
    "set correct to false when the answers disagree."
)


class GradeStatus(StrEnum):
    """Whether a Grader scored the answer or could not."""

    OK = "ok"
    GRADER_ERROR = "grader_error"


@dataclass(frozen=True)
class Grade:
    """One Grader's verdict on one answer.

    ``score`` is None exactly when the status is ``grader_error``.
    """

    score: float | None
    rationale: str
    status: GradeStatus

    def __post_init__(self) -> None:
        if self.status is GradeStatus.GRADER_ERROR and self.score is not None:
            raise ValueError("a grader_error Grade must not carry a score")
        if self.status is GradeStatus.OK and self.score is None:
            raise ValueError("an ok Grade needs a score")


@dataclass(frozen=True)
class GradingInput:
    """Everything the Graders score: the question, the answer and its sources."""

    question: str
    expected_answer: str
    answer: str
    citations: tuple[Citation, ...]
    retrieved: tuple[RetrievedChunk, ...]


class Grader(Protocol):
    """One check that scores an answer on one aspect."""

    name: str

    def grade(self, item: GradingInput) -> Grade:
        """Score one answer, or return a ``grader_error`` Grade."""
        ...


@dataclass(frozen=True)
class GraderSpec:
    """How the registry builds one Grader, and the label the report shows."""

    label: str
    factory: Callable[[Judge | None], Grader]
    requires_judge: bool


class CorrectnessGrader:
    """Compare the answer with the expected answer, by rule then by judge."""

    name = CORRECTNESS

    def __init__(self, judge: Judge):
        self._judge = judge

    def grade(self, item: GradingInput) -> Grade:
        """Return 1 for a normalized match, else ask the judge for a verdict."""
        if not item.expected_answer.strip():
            return _grader_error("no expected answer")
        if normalize_answer(item.answer) == normalize_answer(item.expected_answer):
            return Grade(score=1.0, rationale="normalized match", status=GradeStatus.OK)

        prompt = _CORRECTNESS_PROMPT.format(
            question=item.question,
            expected_answer=item.expected_answer,
            answer=item.answer,
        )
        try:
            reply = self._judge.complete(prompt)
        except (JudgeError, httpx.HTTPError) as exc:
            return _grader_error(f"judge failed: {_excerpt(str(exc))}")

        verdict = _parse_verdict(reply)
        if verdict is None:
            return _grader_error(f"judge reply is not the expected JSON: {_excerpt(reply)}")
        correct, rationale = verdict
        return Grade(score=1.0 if correct else 0.0, rationale=rationale, status=GradeStatus.OK)


def normalize_answer(text: str) -> str:
    """NFKC, casefold, drop source markers, turn punctuation into spaces."""
    normalized = unicodedata.normalize("NFKC", text).casefold()
    normalized = _SOURCE_MARKER.sub(" ", normalized)
    normalized = "".join(
        " " if unicodedata.category(character).startswith("P") else character
        for character in normalized
    )
    return " ".join(normalized.split())


def default_grader_names() -> tuple[str, ...]:
    """Return every registered Grader name, in registry order."""
    return tuple(_REGISTRY)


def grader_label(name: str) -> str:
    """Return the report label for a Grader, or its name when it is unknown."""
    spec = _REGISTRY.get(name)
    return spec.label if spec is not None else name


def build_graders(names: Sequence[str], judge: Judge | None) -> tuple[Grader, ...]:
    """Build the named Graders, rejecting an unknown or repeated name."""
    validate_grader_names(names)
    graders = []
    for name in names:
        spec = _REGISTRY[name]
        if spec.requires_judge and judge is None:
            raise ValueError(f"Grader {name!r} needs a judge model")
        graders.append(spec.factory(judge))
    return tuple(graders)


def validate_grader_names(names: Sequence[str]) -> None:
    """Reject an unknown or repeated Grader name, naming the known ones."""
    unknown = [name for name in names if name not in _REGISTRY]
    if unknown:
        known = ", ".join(_REGISTRY)
        raise ValueError(f"unknown Grader {unknown[0]!r}; known Graders: {known}")
    repeated = sorted({name for name in names if names.count(name) > 1})
    if repeated:
        raise ValueError(f"Grader {repeated[0]!r} was selected twice")


def graders_need_judge(names: Sequence[str]) -> bool:
    """Say whether any selected Grader calls a judge model."""
    validate_grader_names(names)
    return any(_REGISTRY[name].requires_judge for name in names)


def _parse_verdict(reply: str) -> tuple[bool, str] | None:
    """Read ``{"correct": bool, "rationale": str}``, allowing a JSON code fence."""
    text = reply.strip()
    if text.startswith("```"):
        lines = text.splitlines()
        opening = lines[0].strip().lower()
        if len(lines) >= 3 and lines[-1].strip() == "```" and opening in ("```", "```json"):
            text = "\n".join(lines[1:-1]).strip()
    try:
        value: Any = json.loads(text)
    except ValueError:
        return None
    if not isinstance(value, dict):
        return None
    correct = value.get("correct")
    rationale = value.get("rationale")
    if not isinstance(correct, bool) or not isinstance(rationale, str):
        return None
    return correct, rationale


def _grader_error(rationale: str) -> Grade:
    """Return an unscored Grade with its reason."""
    return Grade(score=None, rationale=rationale, status=GradeStatus.GRADER_ERROR)


_REGISTRY: dict[str, GraderSpec] = {
    CORRECTNESS: GraderSpec(
        label="Correctness",
        factory=lambda judge: CorrectnessGrader(judge),
        requires_judge=True,
    ),
}
