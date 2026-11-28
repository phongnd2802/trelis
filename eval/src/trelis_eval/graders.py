"""Graders score one answer on one aspect, and say when they could not.

A Grader never raises for a bad reply or a judge failure: it returns a
``Grade`` with status ``grader_error``, which an Ablation Run excludes from the
mean and counts instead of scoring as zero. A ``not_applicable`` Grade says
the answer gave the Grader nothing to check, which is not a failure. The
registry maps a name to a factory, so the run and the report need no new
plumbing per Grader.
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
FAITHFULNESS = "faithfulness"
CITATION_PRECISION = "citation_precision"

_SOURCE_MARKER = re.compile(r"\[\s*\d+\s*\]")

_MAX_CLAIM_EXCERPT = 100

_CORRECTNESS_PROMPT = (
    "You check whether a candidate answer says the same thing as the expected answer.\n"
    "Question: {question}\n"
    "Expected answer: {expected_answer}\n"
    "Candidate answer: {answer}\n"
    "Reply with JSON only, no prose and no code fence, in this shape: "
    '{{"correct": true, "rationale": "one short sentence"}}; '
    "set correct to false when the answers disagree."
)

_FAITHFULNESS_PROMPT = (
    "You check whether an answer is faithful to the sources it cites.\n"
    "Question: {question}\n"
    "Answer: {answer}\n"
    "Cited sources:\n{sources}\n"
    "List every factual claim the answer makes and say whether the cited sources "
    "support it. If the answer cites no source, every claim is unsupported.\n"
    "Reply with JSON only, no prose and no code fence, in this shape: "
    '{{"claims": [{{"claim": "one factual claim", "supported": true}}], '
    '"rationale": "one short sentence"}}.'
)

_CITATION_PRECISION_PROMPT = (
    "You check whether each source an answer cites supports the statement its marker "
    "is attached to.\n"
    "Answer: {answer}\n"
    "Cited sources:\n{sources}\n"
    "For each cited marker in the answer, say whether that source supports what the "
    "marker is cited for. Judge every cited marker exactly once.\n"
    "Reply with JSON only, no prose and no code fence, in this shape: "
    '{{"citations": [{{"marker": 1, "supported": true}}], '
    '"rationale": "one short sentence"}}.'
)


class GradeStatus(StrEnum):
    """Whether a Grader scored the answer, could not, or had nothing to check."""

    OK = "ok"
    GRADER_ERROR = "grader_error"
    NOT_APPLICABLE = "not_applicable"


@dataclass(frozen=True)
class Grade:
    """One Grader's verdict on one answer.

    ``score`` is None exactly when the status is not ``ok``.
    """

    score: float | None
    rationale: str
    status: GradeStatus

    def __post_init__(self) -> None:
        if self.status is GradeStatus.OK:
            if self.score is None:
                raise ValueError("an ok Grade needs a score")
        elif self.score is not None:
            raise ValueError(f"a {self.status.value} Grade must not carry a score")


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
        """Score one answer, or say it could not or had nothing to check."""
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
            return _not_applicable("no expected answer")
        if normalize_answer(item.answer) == normalize_answer(item.expected_answer):
            return Grade(score=1.0, rationale="normalized match", status=GradeStatus.OK)

        prompt = _CORRECTNESS_PROMPT.format(
            question=item.question,
            expected_answer=item.expected_answer,
            answer=item.answer,
        )
        reply = _ask_judge(self._judge, prompt)
        if isinstance(reply, Grade):
            return reply

        verdict = _parse_verdict(reply)
        if verdict is None:
            return _grader_error(f"judge reply is not the expected JSON: {_excerpt(reply)}")
        correct, rationale = verdict
        return Grade(score=1.0 if correct else 0.0, rationale=rationale, status=GradeStatus.OK)


class FaithfulnessGrader:
    """Score the share of the answer's factual claims its cited sources support."""

    name = FAITHFULNESS

    def __init__(self, judge: Judge):
        self._judge = judge

    def grade(self, item: GradingInput) -> Grade:
        """Ask the judge which claims the cited sources support, and score the share."""
        if not item.answer.strip():
            return _not_applicable("empty answer")
        sources = _cited_sources(item)
        if isinstance(sources, Grade):
            return sources

        prompt = _FAITHFULNESS_PROMPT.format(
            question=item.question,
            answer=item.answer,
            sources=_sources_block(sources),
        )
        reply = _ask_judge(self._judge, prompt)
        if isinstance(reply, Grade):
            return reply

        parsed = _parse_claims(reply)
        if parsed is None:
            return _grader_error(f"judge reply is not the expected JSON: {_excerpt(reply)}")
        claims, judge_rationale = parsed
        if not claims:
            return _not_applicable("answer makes no factual claim")
        supported = sum(1 for _, is_supported in claims if is_supported)
        return Grade(
            score=supported / len(claims),
            rationale=_faithfulness_rationale(claims, supported, judge_rationale),
            status=GradeStatus.OK,
        )


class CitationPrecisionGrader:
    """Score the share of cited markers whose source supports their statement."""

    name = CITATION_PRECISION

    def __init__(self, judge: Judge):
        self._judge = judge

    def grade(self, item: GradingInput) -> Grade:
        """Ask the judge about each distinct cited marker, and score the supported share."""
        if not item.citations:
            return _not_applicable("answer cites no source")
        sources = _cited_sources(item)
        if isinstance(sources, Grade):
            return sources

        cited_markers = sorted({source.marker for source in sources})
        prompt = _CITATION_PRECISION_PROMPT.format(
            answer=item.answer,
            sources=_sources_block(sources),
        )
        reply = _ask_judge(self._judge, prompt)
        if isinstance(reply, Grade):
            return reply

        parsed = _parse_citation_verdicts(reply)
        if parsed is None:
            return _grader_error(f"judge reply is not the expected JSON: {_excerpt(reply)}")
        verdicts, judge_rationale = parsed
        judged_markers = sorted(marker for marker, _ in verdicts)
        if judged_markers != cited_markers:
            return _grader_error(
                "judge reply does not judge each cited marker once: "
                f"cited {cited_markers}, judged {judged_markers}"
            )
        supported = sum(1 for _, is_supported in verdicts if is_supported)
        return Grade(
            score=supported / len(verdicts),
            rationale=(
                f"{supported} of {len(verdicts)} cited sources support what they are "
                f"cited for; {judge_rationale}"
            ),
            status=GradeStatus.OK,
        )


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


def _ask_judge(judge: Judge, prompt: str) -> str | Grade:
    """Return the judge's reply, or the ``grader_error`` Grade the call failed with."""
    try:
        return judge.complete(prompt)
    except (JudgeError, httpx.HTTPError) as exc:
        return _grader_error(f"judge failed: {_excerpt(str(exc))}")


@dataclass(frozen=True)
class _Source:
    """One cited source as the judge sees it, numbered by its citation marker."""

    marker: int
    text: str


def _cited_sources(item: GradingInput) -> tuple[_Source, ...] | Grade:
    """Pair each cited marker with its retrieved Chunk's text, or say why not.

    The text is what the generator saw: the Chunk's context header and content,
    or the content alone when there is no header. A citation whose Chunk is not
    among the retrieved ones, or a marker naming two different Chunks, is a
    ``grader_error``.
    """
    chunks: dict[str, RetrievedChunk] = {chunk.chunk_id: chunk for chunk in item.retrieved}
    marker_chunks: dict[int, str] = {}
    sources: dict[int, str] = {}
    for citation in item.citations:
        chunk = chunks.get(citation.chunk_id)
        if chunk is None:
            return _grader_error(f"citation [{citation.marker}] does not name a retrieved Chunk")
        if citation.marker in marker_chunks and marker_chunks[citation.marker] != citation.chunk_id:
            return _grader_error(f"citation [{citation.marker}] names two different Chunks")
        marker_chunks[citation.marker] = citation.chunk_id
        text = (
            f"{chunk.context_header}\n\n{chunk.content}" if chunk.context_header else chunk.content
        )
        sources.setdefault(citation.marker, text)
    return tuple(_Source(marker=marker, text=text) for marker, text in sources.items())


def _sources_block(sources: Sequence[_Source]) -> str:
    """Render the cited sources for the judge, numbered by marker."""
    if not sources:
        return "(none cited)"
    return "\n".join(f"[{source.marker}] {source.text}" for source in sources)


def _parse_judged_items[JudgedItem](
    reply: str,
    list_key: str,
    read_item: Callable[[dict[str, Any]], JudgedItem | None],
) -> tuple[list[JudgedItem], str] | None:
    """Read ``{"<list_key>": [...], "rationale": str}``, validating every item."""
    value = _parse_json_object(reply)
    if value is None:
        return None
    items = value.get(list_key)
    rationale = value.get("rationale")
    if not isinstance(items, list) or not isinstance(rationale, str):
        return None
    parsed = []
    for item in items:
        if not isinstance(item, dict):
            return None
        read = read_item(item)
        if read is None:
            return None
        parsed.append(read)
    return parsed, rationale


def _read_claim(item: dict[str, Any]) -> tuple[str, bool] | None:
    """Read one claim object, or None when a field has the wrong type."""
    text = item.get("claim")
    supported = item.get("supported")
    if not isinstance(text, str) or not isinstance(supported, bool):
        return None
    return text, supported


def _read_citation_verdict(item: dict[str, Any]) -> tuple[int, bool] | None:
    """Read one citation verdict, or None when a field has the wrong type."""
    marker = item.get("marker")
    supported = item.get("supported")
    if not isinstance(marker, int) or isinstance(marker, bool):
        return None
    if not isinstance(supported, bool):
        return None
    return marker, supported


def _parse_claims(reply: str) -> tuple[list[tuple[str, bool]], str] | None:
    """Read ``{"claims": [{"claim": str, "supported": bool}], "rationale": str}``."""
    return _parse_judged_items(reply, "claims", _read_claim)


def _parse_citation_verdicts(reply: str) -> tuple[list[tuple[int, bool]], str] | None:
    """Read ``{"citations": [{"marker": int, "supported": bool}], "rationale": str}``."""
    return _parse_judged_items(reply, "citations", _read_citation_verdict)


def _faithfulness_rationale(
    claims: Sequence[tuple[str, bool]], supported: int, judge_rationale: str
) -> str:
    """Derive the supported share, naming the first unsupported claim."""
    parts = [f"{supported} of {len(claims)} claims supported"]
    unsupported = next((claim for claim, is_supported in claims if not is_supported), None)
    if unsupported is not None:
        parts.append(f'unsupported: "{_excerpt(unsupported, _MAX_CLAIM_EXCERPT)}"')
    parts.append(judge_rationale)
    return "; ".join(parts)


def _parse_verdict(reply: str) -> tuple[bool, str] | None:
    """Read ``{"correct": bool, "rationale": str}``, allowing a JSON code fence."""
    value = _parse_json_object(reply)
    if value is None:
        return None
    correct = value.get("correct")
    rationale = value.get("rationale")
    if not isinstance(correct, bool) or not isinstance(rationale, str):
        return None
    return correct, rationale


def _parse_json_object(reply: str) -> dict[str, Any] | None:
    """Read a JSON object, allowing it to be wrapped in a JSON code fence."""
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
    return value if isinstance(value, dict) else None


def _grader_error(rationale: str) -> Grade:
    """Return an unscored Grade with its reason."""
    return Grade(score=None, rationale=rationale, status=GradeStatus.GRADER_ERROR)


def _not_applicable(rationale: str) -> Grade:
    """Return an unscored Grade for an answer that gave the Grader nothing to check."""
    return Grade(score=None, rationale=rationale, status=GradeStatus.NOT_APPLICABLE)


_REGISTRY: dict[str, GraderSpec] = {
    CORRECTNESS: GraderSpec(
        label="Correctness",
        factory=CorrectnessGrader,
        requires_judge=True,
    ),
    FAITHFULNESS: GraderSpec(
        label="Faithfulness",
        factory=FaithfulnessGrader,
        requires_judge=True,
    ),
    CITATION_PRECISION: GraderSpec(
        label="Citation precision",
        factory=CitationPrecisionGrader,
        requires_judge=True,
    ),
}
