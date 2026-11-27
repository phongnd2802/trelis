"""Retrieval metrics at Document level (pure functions, no I/O).

The ranked list holds one ``source_id`` per retrieved Chunk, in rank order;
``None`` is a Chunk whose Document is absent from the ingest manifest. The
relevant collection holds the distinct ``source_id`` values a Golden question
cites as evidence. The formulas are documented in ``eval/docs/metrics.md``.
"""

from __future__ import annotations

import math
from collections.abc import Collection, Sequence


def recall_at_k(ranked: Sequence[str | None], relevant: Collection[str], k: int) -> float:
    """Return the share of relevant Documents found among the first ``k`` ranks.

    A Document counts once however many of its Chunks were retrieved; Chunks
    that map to no Document (``None``) are never relevant.
    """
    relevant_set = _relevant_set(relevant)
    _check_k(k)
    found = {
        source_id for source_id in ranked[:k] if source_id is not None and source_id in relevant_set
    }
    return len(found) / len(relevant_set)


def reciprocal_rank(ranked: Sequence[str | None], relevant: Collection[str]) -> float:
    """Return ``1 / rank`` of the first relevant Document, or 0 when none is found."""
    relevant_set = _relevant_set(relevant)
    for rank, source_id in enumerate(ranked, start=1):
        if source_id is not None and source_id in relevant_set:
            return 1.0 / rank
    return 0.0


def ndcg_at_k(ranked: Sequence[str | None], relevant: Collection[str], k: int) -> float:
    """Return binary nDCG@k with one gain per Document, at its first occurrence."""
    relevant_set = _relevant_set(relevant)
    _check_k(k)
    dcg = 0.0
    seen: set[str] = set()
    for rank, source_id in enumerate(ranked[:k], start=1):
        if source_id is None or source_id in seen:
            continue
        seen.add(source_id)
        if source_id in relevant_set:
            dcg += 1.0 / math.log2(rank + 1)
    ideal = sum(1.0 / math.log2(rank + 1) for rank in range(1, min(len(relevant_set), k) + 1))
    return dcg / ideal


def mean(values: Sequence[float]) -> float | None:
    """Return the arithmetic mean, or ``None`` when there is nothing to average."""
    if not values:
        return None
    return sum(values) / len(values)


def nearest_rank_percentile(values: Sequence[float], p: int) -> float | None:
    """Return the nearest-rank percentile, or ``None`` for an empty sequence."""
    if not isinstance(p, int) or isinstance(p, bool) or not 1 <= p <= 100:
        raise ValueError(f"p must be an integer between 1 and 100, got {p!r}")
    if not values:
        return None
    ordered = sorted(values)
    rank = -(-p * len(ordered) // 100)
    return ordered[rank - 1]


def _relevant_set(relevant: Collection[str]) -> set[str]:
    """Return the distinct relevant source_ids, rejecting an empty collection."""
    relevant_set = set(relevant)
    if not relevant_set:
        raise ValueError("relevant must name at least one source_id")
    return relevant_set


def _check_k(k: int) -> None:
    """Reject a cutoff that does not select a rank."""
    if k < 1:
        raise ValueError(f"k must be at least 1, got {k}")
