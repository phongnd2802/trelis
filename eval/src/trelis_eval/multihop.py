"""MultiHop-RAG: fetch it, load it, and turn it into Benchmark Documents and Golden questions."""

from __future__ import annotations

import hashlib
import json
import random
import re
import unicodedata
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx

from .golden import Evidence, GoldenQuestion, hop_count, hops_slice

DATA_BASE_URL = "https://huggingface.co/datasets/yixuantt/MultiHopRAG/resolve/main"
"""Where the published MultiHop-RAG Benchmark Corpus lives."""

_DATA_FILES = ("corpus.json", "MultiHopRAG.json")
_QUESTION_TYPES = frozenset({"inference_query", "comparison_query", "temporal_query", "null_query"})


class MultiHopDataError(ValueError):
    """A MultiHop-RAG file does not have the documented shape."""


@dataclass(frozen=True)
class BenchmarkDocument:
    """One Benchmark Document from the MultiHop-RAG Corpus; its URL is the source_id."""

    source_id: str
    title: str
    author: str | None
    source: str
    published_at: str
    category: str
    body: str


@dataclass(frozen=True)
class EvidenceFact:
    """One fact from a MultiHop-RAG question's evidence_list."""

    title: str
    author: str | None
    url: str
    source: str
    category: str
    published_at: str
    fact: str


@dataclass(frozen=True)
class MultiHopQuestion:
    """One MultiHop-RAG question and the facts that support its answer."""

    query: str
    answer: str
    question_type: str | None
    evidence: tuple[EvidenceFact, ...]


def fetch(dest_dir: str | Path, *, client: httpx.Client | None = None) -> list[Path]:
    """Download both data files into dest_dir, skipping files already there."""
    dest = Path(dest_dir)
    dest.mkdir(parents=True, exist_ok=True)
    owns_client = client is None
    http = client or httpx.Client(follow_redirects=True, timeout=60.0)
    paths: list[Path] = []
    try:
        for name in _DATA_FILES:
            path = dest / name
            if not path.exists():
                response = http.get(f"{DATA_BASE_URL}/{name}")
                response.raise_for_status()
                path.write_bytes(response.content)
            paths.append(path)
    finally:
        if owns_client:
            http.close()
    return paths


def load_corpus(path: str | Path) -> list[BenchmarkDocument]:
    """Load corpus.json, failing on a bad field or a duplicate URL."""
    path = Path(path)
    documents: list[BenchmarkDocument] = []
    first_document_by_url: dict[str, int] = {}
    for index, item in enumerate(_load_list(path, "documents")):
        where = f"{path}: document {index}"
        if not isinstance(item, dict):
            raise MultiHopDataError(f"{where}: expected a JSON object")
        source_id = _required_string(item, "url", where)
        if source_id in first_document_by_url:
            first = first_document_by_url[source_id]
            raise MultiHopDataError(
                f"{where}: duplicate url {source_id!r} (first at document {first})"
            )
        first_document_by_url[source_id] = index
        documents.append(
            BenchmarkDocument(
                source_id=source_id,
                title=_required_string(item, "title", where),
                author=_nullable_string(item, "author", where),
                source=_required_string(item, "source", where),
                published_at=_required_string(item, "published_at", where),
                category=_required_string(item, "category", where),
                body=_required_string(item, "body", where),
            )
        )
    return documents


def load_questions(path: str | Path) -> list[MultiHopQuestion]:
    """Load MultiHopRAG.json, failing on a bad field or question type."""
    path = Path(path)
    questions: list[MultiHopQuestion] = []
    for index, item in enumerate(_load_list(path, "questions")):
        where = f"{path}: question {index}"
        if not isinstance(item, dict):
            raise MultiHopDataError(f"{where}: expected a JSON object")
        questions.append(
            MultiHopQuestion(
                query=_required_string(item, "query", where),
                answer=_string(item, "answer", where),
                question_type=_question_type(item, where),
                evidence=_load_evidence(item, path, index),
            )
        )
    return questions


def to_golden_set(questions: list[MultiHopQuestion]) -> list[GoldenQuestion]:
    """Map MultiHop-RAG questions onto the Golden Set format, in file order."""
    golden: list[GoldenQuestion] = []
    for index, question in enumerate(questions):
        distinct_urls = {evidence.url for evidence in question.evidence}
        slices: list[str] = []
        if question.question_type is not None:
            slices.append(question.question_type)
        slices.append(hops_slice(len(distinct_urls)))
        golden.append(
            GoldenQuestion(
                id=f"mhr-{index:04d}",
                question=question.query,
                expected_answer=question.answer,
                evidence=tuple(
                    Evidence(source_id=evidence.url, fact=evidence.fact)
                    for evidence in question.evidence
                ),
                slices=tuple(slices),
            )
        )
    return golden


def select_subset(
    documents: list[BenchmarkDocument],
    golden: list[GoldenQuestion],
    questions: int,
    extra_docs: int,
    seed: int,
) -> tuple[list[GoldenQuestion], list[BenchmarkDocument]]:
    """Pick the first questions, their evidence documents and seeded distractors."""
    if questions < 0 or extra_docs < 0:
        raise ValueError("questions and extra_docs must not be negative")
    if questions > len(golden):
        raise ValueError(f"asked for {questions} Golden questions but only {len(golden)} exist")

    selected_questions = list(golden[:questions])
    return selected_questions, select_evidence_documents(
        documents, selected_questions, extra_docs, seed
    )


def stratified_sample(golden: Sequence[GoldenQuestion], n: int, seed: int) -> list[GoldenQuestion]:
    """Pick ``n`` questions proportionally from the question_type Slices.

    Allocation uses the largest remainder method over each question's single
    non-``hops-*`` Slice, with ties broken by Slice name. Within each Slice the
    questions are sorted by id before one ``random.Random(seed)`` stream
    samples them. The union is returned sorted by id.
    """
    if n < 0:
        raise ValueError("n must not be negative")
    if n > len(golden):
        raise ValueError(f"asked for {n} Golden questions but only {len(golden)} exist")

    slices_by_type = _slices_by_question_type(golden)
    total = len(golden)
    allocation = {name: n * len(questions) // total for name, questions in slices_by_type.items()}
    remainders = {name: n * len(questions) % total for name, questions in slices_by_type.items()}
    left = n - sum(allocation.values())
    for name in sorted(slices_by_type, key=lambda name: (-remainders[name], name))[:left]:
        allocation[name] += 1

    selected: list[GoldenQuestion] = []
    rng = random.Random(seed)
    for name in sorted(slices_by_type):
        pool = sorted(slices_by_type[name], key=lambda question: question.id)
        selected.extend(rng.sample(pool, allocation[name]))
    return sorted(selected, key=lambda question: question.id)


def select_evidence_documents(
    documents: list[BenchmarkDocument],
    questions: list[GoldenQuestion],
    extra_docs: int,
    seed: int,
) -> list[BenchmarkDocument]:
    """Include every evidence Document of ``questions`` plus seeded distractors."""
    if extra_docs < 0:
        raise ValueError("extra_docs must not be negative")
    wanted = {evidence.source_id for question in questions for evidence in question.evidence}
    by_source_id = {document.source_id: document for document in documents}
    missing = sorted(wanted - set(by_source_id))
    if missing:
        raise ValueError(
            "the Golden Set cites evidence missing from the Corpus: " + ", ".join(missing)
        )

    cited = [document for document in documents if document.source_id in wanted]
    others = [document for document in documents if document.source_id not in wanted]
    if extra_docs > len(others):
        raise ValueError(
            f"not enough documents for {extra_docs} distractors: only {len(others)} left"
        )
    distractors = random.Random(seed).sample(others, extra_docs)
    return cited + distractors


def _slices_by_question_type(
    golden: Sequence[GoldenQuestion],
) -> dict[str, list[GoldenQuestion]]:
    """Group questions by the one Slice that names their question type."""
    slices_by_type: dict[str, list[GoldenQuestion]] = {}
    for question in golden:
        names = [name for name in question.slices if hop_count(name) is None]
        if len(names) != 1:
            raise ValueError(
                f"Golden question {question.id!r} must have exactly one non-hops-* "
                f"Slice naming its question type, got {names!r}"
            )
        slices_by_type.setdefault(names[0], []).append(question)
    return slices_by_type


def document_markdown(document: BenchmarkDocument) -> tuple[str, bytes]:
    """Return the upload file name and Markdown body for one Benchmark Document.

    The body is deterministic, so re-running an ingest produces the same
    bytes and Trelis's duplicate detection returns the earlier Document.
    """
    metadata = "\n".join(
        [
            f"- Source: {document.source}",
            f"- Published: {document.published_at}",
            f"- Category: {document.category}",
            f"- URL: {document.source_id}",
        ]
    )
    markdown = f"# {document.title}\n\n{metadata}\n\n{document.body}\n"
    digest = hashlib.sha256(document.source_id.encode("utf-8")).hexdigest()[:8]
    return f"{_slug(document.title)}-{digest}.md", markdown.encode("utf-8")


def _load_list(path: Path, what: str) -> list[Any]:
    """Decode a JSON list, naming the file when it is not one."""
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise MultiHopDataError(
            f"{path}: invalid JSON: {exc.msg} (line {exc.lineno}, column {exc.colno})"
        ) from exc
    if not isinstance(value, list):
        raise MultiHopDataError(f"{path}: expected a JSON list of {what}")
    return value


def _required_string(item: dict[str, Any], name: str, where: str) -> str:
    """Return a non-empty string field or fail naming file, index and field."""
    if name not in item:
        raise MultiHopDataError(f"{where}: missing field {name!r}")
    value = item[name]
    if not isinstance(value, str):
        raise MultiHopDataError(f"{where}: field {name!r} must be a string")
    if not value.strip():
        raise MultiHopDataError(f"{where}: field {name!r} must not be empty")
    return value


def _string(item: dict[str, Any], name: str, where: str) -> str:
    """Return a string field, which may be empty, or fail naming it."""
    if name not in item:
        raise MultiHopDataError(f"{where}: missing field {name!r}")
    value = item[name]
    if not isinstance(value, str):
        raise MultiHopDataError(f"{where}: field {name!r} must be a string")
    return value


def _nullable_string(item: dict[str, Any], name: str, where: str) -> str | None:
    """Return a string-or-null field, or fail naming it."""
    if name not in item:
        raise MultiHopDataError(f"{where}: missing field {name!r}")
    value = item[name]
    if value is None or isinstance(value, str):
        return value
    raise MultiHopDataError(f"{where}: field {name!r} must be a string or null")


def _question_type(item: dict[str, Any], where: str) -> str | None:
    """Return the question_type, which may be null, or fail naming it."""
    if "question_type" not in item:
        raise MultiHopDataError(f"{where}: missing field 'question_type'")
    value = item["question_type"]
    if value is None:
        return None
    if not isinstance(value, str) or value not in _QUESTION_TYPES:
        allowed = ", ".join(sorted(_QUESTION_TYPES)) + " or null"
        raise MultiHopDataError(f"{where}: field 'question_type' must be one of {allowed}")
    return value


def _load_evidence(
    item: dict[str, Any], path: Path, question_index: int
) -> tuple[EvidenceFact, ...]:
    """Validate one question's evidence_list, which may be empty."""
    where = f"{path}: question {question_index}"
    if "evidence_list" not in item:
        raise MultiHopDataError(f"{where}: missing field 'evidence_list'")
    raw_evidence = item["evidence_list"]
    if not isinstance(raw_evidence, list):
        raise MultiHopDataError(f"{where}: field 'evidence_list' must be a list")

    evidence: list[EvidenceFact] = []
    for index, fact in enumerate(raw_evidence):
        fact_where = f"{where}: evidence {index}"
        if not isinstance(fact, dict):
            raise MultiHopDataError(f"{fact_where}: expected a JSON object")
        evidence.append(
            EvidenceFact(
                title=_required_string(fact, "title", fact_where),
                author=_nullable_string(fact, "author", fact_where),
                url=_required_string(fact, "url", fact_where),
                source=_required_string(fact, "source", fact_where),
                category=_required_string(fact, "category", fact_where),
                published_at=_required_string(fact, "published_at", fact_where),
                fact=_required_string(fact, "fact", fact_where),
            )
        )
    return tuple(evidence)


def _slug(title: str) -> str:
    """Make a filesystem-safe, ASCII-only slug of a title."""
    ascii_title = unicodedata.normalize("NFKD", title).encode("ascii", "ignore").decode("ascii")
    slug = re.sub(r"[^a-z0-9]+", "-", ascii_title.lower()).strip("-")[:60].strip("-")
    return slug or "document"
