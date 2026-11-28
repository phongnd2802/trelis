"""Label one scored answer by the stage that failed.

A wrong answer is a Retrieval miss when one of the evidence Documents it needs
was never retrieved, and a Generation error otherwise. A correct answer is a
Citation error when its citations are missing or partly unsupported. A Grade
that could not judge the answer leaves the question Unattributed.
``eval/docs/metrics.md`` holds the rules.
"""

from __future__ import annotations

from enum import StrEnum

from .ablation_run import QuestionResult
from .graders import CITATION_PRECISION, CORRECTNESS, GradeStatus


class FailureLabel(StrEnum):
    """The one stage a scored answer's outcome is attributed to."""

    CORRECT = "correct"
    RETRIEVAL_MISS = "retrieval_miss"
    GENERATION_ERROR = "generation_error"
    CITATION_ERROR = "citation_error"
    UNATTRIBUTED = "unattributed"


def attribute_failure(result: QuestionResult) -> FailureLabel:
    """Return the one label of a scored answer.

    Without a usable correctness Grade there is no outcome to attribute. A
    wrong answer is a Retrieval miss when any evidence ``source_id`` is absent
    from the whole retrieved list, and a Generation error otherwise. A correct
    answer is a Citation error when citation precision is below 1.0, including
    an answer that cites nothing; a missing or failed citation Grade leaves it
    Unattributed because the citations could not be checked.
    """
    correctness = result.grades.get(CORRECTNESS)
    if correctness is None or correctness.status is not GradeStatus.OK:
        return FailureLabel.UNATTRIBUTED
    if correctness.score < 1.0:
        retrieved = {record.source_id for record in result.retrieved}
        if any(source_id not in retrieved for source_id in result.evidence):
            return FailureLabel.RETRIEVAL_MISS
        return FailureLabel.GENERATION_ERROR

    citation = result.grades.get(CITATION_PRECISION)
    if citation is None or citation.status is GradeStatus.GRADER_ERROR:
        return FailureLabel.UNATTRIBUTED
    if citation.status is GradeStatus.NOT_APPLICABLE or citation.score < 1.0:
        return FailureLabel.CITATION_ERROR
    return FailureLabel.CORRECT
