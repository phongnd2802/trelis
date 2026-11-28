"""Tests for the Failure Attribution label of one scored answer."""

from __future__ import annotations

from trelis_eval.ablation_run import (
    QuestionMetrics,
    QuestionResult,
    QuestionStatus,
    RetrievedRecord,
)
from trelis_eval.attribution import FailureLabel, attribute_failure
from trelis_eval.graders import CITATION_PRECISION, CORRECTNESS, Grade, GradeStatus

EVIDENCE = "https://example.com/alpha"
OTHER_EVIDENCE = "https://example.com/beta"


def ok(score: float) -> Grade:
    return Grade(score=score, rationale="graded", status=GradeStatus.OK)


def failed() -> Grade:
    return Grade(score=None, rationale="judge failed", status=GradeStatus.GRADER_ERROR)


def not_applicable() -> Grade:
    return Grade(score=None, rationale="nothing to check", status=GradeStatus.NOT_APPLICABLE)


def result(
    *,
    correctness: Grade | None = None,
    citation: Grade | None = None,
    evidence: tuple[str, ...] = (EVIDENCE,),
    retrieved: tuple[str | None, ...] = (EVIDENCE,),
) -> QuestionResult:
    """Build one scored answer from its Grades and its retrieved Documents."""
    grades = {}
    if correctness is not None:
        grades[CORRECTNESS] = correctness
    if citation is not None:
        grades[CITATION_PRECISION] = citation
    return QuestionResult(
        id="q1",
        slices=("inference_query",),
        status=QuestionStatus.SCORED,
        evidence=evidence,
        retrieved=tuple(
            RetrievedRecord(
                chunk_id=f"chunk-{index}",
                document_id=f"document-{index}",
                source_id=source_id,
                score=1.0,
                scores={},
            )
            for index, source_id in enumerate(retrieved)
        ),
        trace=(),
        metrics=QuestionMetrics(1.0, 1.0, 1.0, 1.0),
        latency_ms=10.0,
        answer="An answer.",
        grades=grades,
    )


def test_a_missing_correctness_grade_is_unattributed():
    assert attribute_failure(result()) is FailureLabel.UNATTRIBUTED


def test_a_correctness_grader_error_is_unattributed():
    assert attribute_failure(result(correctness=failed())) is FailureLabel.UNATTRIBUTED


def test_a_not_applicable_correctness_grade_is_unattributed():
    assert attribute_failure(result(correctness=not_applicable())) is FailureLabel.UNATTRIBUTED


def test_a_wrong_answer_missing_one_of_two_evidence_documents_is_a_retrieval_miss():
    assert (
        attribute_failure(
            result(
                correctness=ok(0.0),
                evidence=(EVIDENCE, OTHER_EVIDENCE),
                retrieved=(EVIDENCE,),
            )
        )
        is FailureLabel.RETRIEVAL_MISS
    )


def test_a_wrong_answer_with_every_evidence_document_retrieved_is_a_generation_error():
    assert (
        attribute_failure(
            result(
                correctness=ok(0.0),
                evidence=(EVIDENCE, OTHER_EVIDENCE),
                retrieved=(EVIDENCE, OTHER_EVIDENCE),
            )
        )
        is FailureLabel.GENERATION_ERROR
    )


def test_a_wrong_answer_with_no_evidence_is_a_generation_error():
    assert (
        attribute_failure(result(correctness=ok(0.0), evidence=(), retrieved=()))
        is FailureLabel.GENERATION_ERROR
    )


def test_a_correct_answer_with_imperfect_citation_precision_is_a_citation_error():
    assert (
        attribute_failure(result(correctness=ok(1.0), citation=ok(0.5)))
        is FailureLabel.CITATION_ERROR
    )


def test_a_correct_answer_that_cites_nothing_is_a_citation_error():
    assert (
        attribute_failure(result(correctness=ok(1.0), citation=not_applicable()))
        is FailureLabel.CITATION_ERROR
    )


def test_a_correct_answer_with_perfect_citation_precision_is_correct():
    assert attribute_failure(result(correctness=ok(1.0), citation=ok(1.0))) is FailureLabel.CORRECT


def test_a_correct_answer_whose_citation_grade_failed_is_unattributed():
    assert (
        attribute_failure(result(correctness=ok(1.0), citation=failed()))
        is FailureLabel.UNATTRIBUTED
    )


def test_a_correct_answer_without_a_citation_grade_is_unattributed():
    assert attribute_failure(result(correctness=ok(1.0))) is FailureLabel.UNATTRIBUTED


def test_evidence_retrieved_beyond_the_first_ten_is_still_retrieved():
    beyond_ten = tuple(f"https://example.com/unrelated-{index}" for index in range(10))

    label = attribute_failure(
        result(correctness=ok(0.0), evidence=(EVIDENCE,), retrieved=(*beyond_ten, EVIDENCE))
    )

    assert label is FailureLabel.GENERATION_ERROR
