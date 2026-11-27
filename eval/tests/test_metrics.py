"""Hand-computed cases for the retrieval metrics (pure functions, no I/O)."""

import math

import pytest

from trelis_eval.metrics import (
    mean,
    ndcg_at_k,
    nearest_rank_percentile,
    recall_at_k,
    reciprocal_rank,
)

# Ranked: [a, x, b, a, None]; relevant Documents: a, b, c.
RANKED = ("a", "x", "b", "a", None)
RELEVANT = frozenset({"a", "b", "c"})


def test_recall_counts_distinct_relevant_documents_in_the_first_k():
    assert recall_at_k(RANKED, RELEVANT, 1) == 1 / 3
    assert recall_at_k(RANKED, RELEVANT, 5) == 2 / 3


def test_recall_at_a_k_larger_than_the_ranked_list_uses_the_whole_list():
    assert recall_at_k(("a",), RELEVANT, 10) == 1 / 3


def test_recall_is_zero_when_no_relevant_document_is_retrieved():
    assert recall_at_k(("x", "y", None), RELEVANT, 5) == 0.0


def test_recall_ignores_relevant_documents_beyond_k():
    assert recall_at_k(("x", "a"), {"a"}, 1) == 0.0


def test_recall_counts_a_document_once_however_many_chunks_it_has():
    # Two Chunks of Document a are worth one of the two relevant Documents.
    assert recall_at_k(("a", "a"), {"a", "b"}, 2) == 1 / 2


def test_reciprocal_rank_is_the_inverse_of_the_first_relevant_rank():
    assert reciprocal_rank(RANKED, RELEVANT) == 1.0


def test_reciprocal_rank_uses_the_first_relevant_document_it_finds():
    assert reciprocal_rank(("x", "y", "b", "a"), {"a", "b"}) == 1 / 3


def test_reciprocal_rank_is_zero_when_nothing_relevant_is_retrieved():
    assert reciprocal_rank(("x", None), {"a"}) == 0.0


def test_ndcg_matches_the_hand_computation_for_duplicates_and_none():
    # Gains at ranks 1 (a) and 3 (b); the second a at rank 4 gains 0 and
    # None at rank 5 gains 0. IDCG places the three relevant Documents first.
    dcg = 1 / math.log2(2) + 1 / math.log2(4)
    idcg = 1 / math.log2(2) + 1 / math.log2(3) + 1 / math.log2(4)

    assert ndcg_at_k(RANKED, RELEVANT, 5) == pytest.approx(dcg / idcg)


def test_ndcg_at_a_k_larger_than_the_ranked_list_uses_the_whole_list():
    dcg = 1 / math.log2(2)
    idcg = 1 / math.log2(2) + 1 / math.log2(3) + 1 / math.log2(4)

    assert ndcg_at_k(("a",), RELEVANT, 10) == pytest.approx(dcg / idcg)


def test_ndcg_is_zero_when_no_relevant_document_is_retrieved():
    assert ndcg_at_k(("x", "y", None), RELEVANT, 5) == 0.0


def test_ndcg_ignores_relevant_documents_beyond_k():
    assert ndcg_at_k(("x", "a"), {"a"}, 1) == 0.0


def test_ndcg_counts_only_the_first_chunk_of_a_document():
    # a at rank 1 gains 1/log2(2); its repeat at rank 2 gains 0; b is
    # relevant but not retrieved, so IDCG is 1/log2(2) + 1/log2(3).
    expected = (1 / math.log2(2)) / (1 / math.log2(2) + 1 / math.log2(3))

    assert ndcg_at_k(("a", "a"), {"a", "b"}, 2) == pytest.approx(expected)


def test_an_empty_relevant_collection_is_rejected_by_every_metric():
    with pytest.raises(ValueError, match="relevant"):
        recall_at_k(("a",), (), 5)
    with pytest.raises(ValueError, match="relevant"):
        reciprocal_rank(("a",), ())
    with pytest.raises(ValueError, match="relevant"):
        ndcg_at_k(("a",), (), 5)


@pytest.mark.parametrize("k", [0, -1])
def test_k_below_one_is_rejected(k):
    with pytest.raises(ValueError, match="k"):
        recall_at_k(("a",), {"a"}, k)
    with pytest.raises(ValueError, match="k"):
        ndcg_at_k(("a",), {"a"}, k)


def test_mean_is_none_for_no_values_and_otherwise_the_arithmetic_mean():
    assert mean([]) is None
    assert mean([1.0, 2.0]) == 1.5


def test_nearest_rank_percentile_picks_the_ceiling_integer_rank():
    # ceil(7 * 100 / 100) = 7, without float rounding landing on rank 8.
    assert nearest_rank_percentile(list(range(1, 101)), 7) == 7
    # ceil(0.50 * 4) = 2, so the second of the sorted values.
    assert nearest_rank_percentile([40, 10, 30, 20], 50) == 20
    # ceil(0.95 * 4) = 4.
    assert nearest_rank_percentile([40, 10, 30, 20], 95) == 40
    # ceil(0.50 * 3) = 2.
    assert nearest_rank_percentile([30, 10, 20], 50) == 20


def test_nearest_rank_percentile_is_none_for_no_values():
    assert nearest_rank_percentile([], 95) is None


@pytest.mark.parametrize("p", [0, -1, 101, 50.5])
def test_nearest_rank_percentile_rejects_a_percentage_not_an_integer_in_1_to_100(p):
    with pytest.raises(ValueError, match="p"):
        nearest_rank_percentile([1.0], p)
