"""The Golden Set format: one validated question per JSONL line.

The format is documented in ``eval/docs/golden-set.md``. Loading is strict so
a typo in a hand-edited Golden Set fails before an Ablation Run spends money.
"""

from __future__ import annotations

import json
import re
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class Evidence:
    """One fact in a Source File that supports a Golden question's answer."""

    source_id: str
    fact: str


@dataclass(frozen=True)
class GoldenQuestion:
    """One question with its expected answer, evidence and Slices."""

    id: str
    question: str
    expected_answer: str
    evidence: tuple[Evidence, ...]
    slices: tuple[str, ...]
    principal: str | None = None


class GoldenSetError(ValueError):
    """One or more invalid lines in a Golden Set file.

    ``errors`` holds one ``path:line: message`` string per problem, so every
    mistake in a run's Golden Set is fixed in one pass.
    """

    def __init__(self, errors: Iterable[str]):
        self.errors = list(errors)
        super().__init__("\n".join(self.errors))


_REQUIRED_FIELDS = ("id", "question", "expected_answer", "evidence", "slices")
_OPTIONAL_FIELDS = ("principal",)
_ALLOWED_FIELDS = _REQUIRED_FIELDS + _OPTIONAL_FIELDS
_EVIDENCE_FIELDS = ("source_id", "fact")
_HOPS_SLICE = re.compile(r"hops-(\d+)")


def hops_slice(n: int) -> str:
    """Return the Slice name a question with ``n`` evidence Documents carries."""
    return f"hops-{n}"


def hop_count(slice_name: str) -> int | None:
    """Return the hop count of a ``hops-N`` Slice, or None for another Slice."""
    match = _HOPS_SLICE.fullmatch(slice_name)
    return int(match.group(1)) if match is not None else None


def load_golden_set(path: str | Path) -> list[GoldenQuestion]:
    """Load a Golden Set, raising :class:`GoldenSetError` for any bad line."""
    path = Path(path)
    problems: list[str] = []
    questions: list[GoldenQuestion] = []
    first_line_by_id: dict[str, int] = {}

    for line_number, raw_line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        where = f"{path}:{line_number}"
        if not raw_line.strip():
            problems.append(f"{where}: blank line (one JSON object per line)")
            continue
        try:
            value = json.loads(raw_line)
        except json.JSONDecodeError as exc:
            problems.append(f"{where}: invalid JSON: {exc.msg}")
            continue
        question = _parse_question(value, where, problems)
        if question is None:
            continue
        if question.id in first_line_by_id:
            first_line = first_line_by_id[question.id]
            problems.append(f"{where}: duplicate id {question.id!r} (first on line {first_line})")
            continue
        first_line_by_id[question.id] = line_number
        questions.append(question)

    if problems:
        raise GoldenSetError(problems)
    return questions


def write_golden_set(path: str | Path, questions: Iterable[GoldenQuestion]) -> None:
    """Write Golden questions as JSONL; loading the file returns them unchanged."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as out:
        for question in questions:
            out.write(json.dumps(_question_body(question), ensure_ascii=False) + "\n")


def _parse_question(value: Any, where: str, problems: list[str]) -> GoldenQuestion | None:
    """Validate one decoded line, appending every problem it has."""
    errors_before = len(problems)

    def add(message: str) -> None:
        problems.append(f"{where}: {message}")

    if not isinstance(value, dict):
        add("expected a JSON object")
        return None

    unknown = sorted(set(value) - set(_ALLOWED_FIELDS))
    if unknown:
        add(f"unknown field(s): {', '.join(repr(name) for name in unknown)}")
    for name in _REQUIRED_FIELDS:
        if name not in value:
            add(f"missing field {name!r}")

    question_id = _string_field(value, "id", add)
    question = _string_field(value, "question", add)
    expected_answer = _string_field(value, "expected_answer", add, allow_empty=True)
    evidence = _evidence_field(value, add)
    slices = _slices_field(value, add)
    principal = _principal_field(value, add)

    if len(problems) > errors_before:
        return None
    return GoldenQuestion(
        id=question_id,
        question=question,
        expected_answer=expected_answer,
        evidence=evidence,
        slices=slices,
        principal=principal,
    )


def _string_field(
    value: dict[str, Any],
    name: str,
    add: Callable[[str], None],
    *,
    allow_empty: bool = False,
) -> str | None:
    """Return a required string field, or record why it is invalid."""
    if name not in value:
        return None
    field = value[name]
    if not isinstance(field, str):
        add(f"field {name!r} must be a string")
        return None
    if not allow_empty and not field.strip():
        add(f"field {name!r} must not be empty")
        return None
    return field


def _evidence_field(value: dict[str, Any], add: Callable[[str], None]) -> tuple[Evidence, ...]:
    """Validate the evidence list, which may be empty but not malformed."""
    if "evidence" not in value:
        return ()
    raw_evidence = value["evidence"]
    if not isinstance(raw_evidence, list):
        add("field 'evidence' must be a list")
        return ()

    evidence: list[Evidence] = []
    for index, item in enumerate(raw_evidence):
        label = f"evidence[{index}]"
        if not isinstance(item, dict):
            add(f"field {label!r} must be a JSON object")
            continue
        unknown = sorted(set(item) - set(_EVIDENCE_FIELDS))
        if unknown:
            add(
                f"field {label!r} has unknown field(s): {', '.join(repr(name) for name in unknown)}"
            )
        source_id = item.get("source_id")
        fact = item.get("fact")
        if not isinstance(source_id, str) or not source_id.strip():
            add(f"field {label!r} needs a non-empty 'source_id' string")
        if not isinstance(fact, str) or not fact.strip():
            add(f"field {label!r} needs a non-empty 'fact' string")
        if (
            isinstance(source_id, str)
            and source_id.strip()
            and isinstance(fact, str)
            and fact.strip()
        ):
            evidence.append(Evidence(source_id=source_id, fact=fact))
    return tuple(evidence)


def _slices_field(value: dict[str, Any], add: Callable[[str], None]) -> tuple[str, ...]:
    """Validate that every Slice is a non-empty string."""
    if "slices" not in value:
        return ()
    raw_slices = value["slices"]
    if not isinstance(raw_slices, list):
        add("field 'slices' must be a list")
        return ()

    slices: list[str] = []
    for index, item in enumerate(raw_slices):
        if not isinstance(item, str) or not item.strip():
            add(f"field 'slices[{index}]' must be a non-empty string")
        else:
            slices.append(item)
    return tuple(slices)


def _principal_field(value: dict[str, Any], add: Callable[[str], None]) -> str | None:
    """Return the optional Principal name, or record why it is invalid."""
    if "principal" not in value:
        return None
    principal = value["principal"]
    if not isinstance(principal, str) or not principal.strip():
        add("field 'principal' must be a non-empty string when present")
        return None
    return principal


def _question_body(question: GoldenQuestion) -> dict[str, Any]:
    """Turn one Golden question into the JSON object written on its line."""
    body: dict[str, Any] = {
        "id": question.id,
        "question": question.question,
        "expected_answer": question.expected_answer,
        "evidence": [
            {"source_id": evidence.source_id, "fact": evidence.fact}
            for evidence in question.evidence
        ],
        "slices": list(question.slices),
    }
    if question.principal is not None:
        body["principal"] = question.principal
    return body
