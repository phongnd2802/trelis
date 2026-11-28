"""Tests for the Grader registry, the correctness Grader and the judge-based Graders."""

import json

import httpx
import pytest
from fakes import ScriptedJudge

from trelis_eval.client import Citation, RetrievedChunk
from trelis_eval.graders import (
    CitationPrecisionGrader,
    CorrectnessGrader,
    FaithfulnessGrader,
    Grade,
    GradeStatus,
    GradingInput,
    build_graders,
    default_grader_names,
    normalize_answer,
)
from trelis_eval.judge import JudgeClient, JudgeError


def grading_input(answer, expected_answer="Paris", **overrides):
    values = {
        "question": "What is the capital of France?",
        "expected_answer": expected_answer,
        "answer": answer,
        "citations": (),
        "retrieved": (),
    }
    values.update(overrides)
    return GradingInput(**values)


def retrieved_chunk(chunk_id, content="The Alpha Mission launched in 2024.", context_header=""):
    return RetrievedChunk(
        chunk_id=chunk_id,
        document_id="doc-a",
        seq=0,
        content=content,
        context_header=context_header,
        score=0.9,
        scores={"dense": 0.9},
    )


def citation(marker, chunk_id):
    return Citation(marker=marker, chunk_id=chunk_id, document_id="doc-a")


def claims_reply(*claims, rationale="checked"):
    return json.dumps(
        {
            "claims": [{"claim": claim, "supported": supported} for claim, supported in claims],
            "rationale": rationale,
        }
    )


def citations_reply(*citations, rationale="checked"):
    return json.dumps(
        {
            "citations": [
                {"marker": marker, "supported": supported} for marker, supported in citations
            ],
            "rationale": rationale,
        }
    )


def test_a_normalized_match_scores_one_without_calling_the_judge():
    judge = ScriptedJudge('{"correct": false, "rationale": "must not be asked"}')

    grade = CorrectnessGrader(judge).grade(grading_input("  PARIS. [1]"))

    assert grade == Grade(
        score=1.0,
        rationale="normalized match",
        status=GradeStatus.OK,
    )
    assert judge.prompts == []


def test_the_judge_says_correct_scores_one():
    judge = ScriptedJudge('{"correct": true, "rationale": "same city"}')

    grade = CorrectnessGrader(judge).grade(grading_input("The capital is Paris, France."))

    assert grade.status is GradeStatus.OK
    assert grade.score == 1.0


def test_the_judge_says_incorrect_scores_zero():
    judge = ScriptedJudge('{"correct": false, "rationale": "different city"}')

    grade = CorrectnessGrader(judge).grade(grading_input("Lyon"))

    assert grade.status is GradeStatus.OK
    assert grade.score == 0.0


def test_a_fenced_json_reply_is_accepted():
    judge = ScriptedJudge('```json\n{"correct": true, "rationale": "same city"}\n```')

    grade = CorrectnessGrader(judge).grade(grading_input("Paris, France."))

    assert grade.status is GradeStatus.OK
    assert grade.score == 1.0


def test_a_reply_with_extra_keys_is_accepted():
    judge = ScriptedJudge('{"correct": true, "rationale": "same city", "confidence": 0.9}')

    grade = CorrectnessGrader(judge).grade(grading_input("Paris, France."))

    assert grade.status is GradeStatus.OK
    assert grade.score == 1.0


@pytest.mark.parametrize(
    "reply",
    [
        "yes, it is correct",
        '{"verdict": "correct"}',
        '{"correct": "yes", "rationale": "wrong type"}',
        '{"correct": true}',
        "[true]",
    ],
)
def test_a_reply_that_is_not_the_expected_json_is_a_grader_error(reply):
    judge = ScriptedJudge(reply)

    grade = CorrectnessGrader(judge).grade(grading_input("Paris, France."))

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "judge reply" in grade.rationale


def test_a_judge_http_error_is_a_grader_error():
    judge = ScriptedJudge(error=JudgeError("judge answered HTTP 500: boom"))

    grade = CorrectnessGrader(judge).grade(grading_input("Paris, France."))

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "HTTP 500" in grade.rationale


def test_a_judge_timeout_is_a_grader_error():
    judge = ScriptedJudge(error=httpx.ReadTimeout("judge timed out"))

    grade = CorrectnessGrader(judge).grade(grading_input("Paris, France."))

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "judge" in grade.rationale.lower()


def test_an_empty_expected_answer_is_not_applicable_without_calling_the_judge():
    judge = ScriptedJudge('{"correct": true, "rationale": "unused"}')

    grade = CorrectnessGrader(judge).grade(grading_input("Paris", expected_answer="  "))

    assert grade == Grade(
        score=None,
        rationale="no expected answer",
        status=GradeStatus.NOT_APPLICABLE,
    )
    assert judge.prompts == []


def test_a_long_unparseable_reply_is_cut_in_the_rationale():
    judge = ScriptedJudge("x" * 500)

    grade = CorrectnessGrader(judge).grade(grading_input("Paris, France."))

    assert grade.status is GradeStatus.GRADER_ERROR
    assert "x" * 200 in grade.rationale
    assert "x" * 201 not in grade.rationale


def test_a_judge_that_fails_three_attempts_becomes_a_grader_error():
    calls = []

    def handler(request):
        calls.append(request)
        return httpx.Response(500, json={"error": {"message": "judge is down"}})

    judge = JudgeClient(
        "http://judge.test",
        "judge-key",
        "judge-model",
        transport=httpx.MockTransport(handler),
        sleep=lambda _: None,
    )

    grade = CorrectnessGrader(judge).grade(grading_input("Paris, France."))

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "HTTP 500" in grade.rationale
    assert len(calls) == 3


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("ＡＢＣ", "abc"),
        ("ﬁle", "file"),
        ("Straße", "strasse"),
        ("U.S.A", "u s a"),
        ("[ 12 ]  [3] answer", "answer"),
        ("Thuế TNCN", "thuế tncn"),
        ("  Hello,   WORLD!  ", "hello world"),
    ],
)
def test_normalize_answer(text, expected):
    assert normalize_answer(text) == expected


def test_normalize_answer_folds_vietnamese_case_without_dropping_diacritics():
    assert normalize_answer("Thuế TNCN") == normalize_answer("thuế tncn")


def test_normalize_answer_matches_inner_punctuation_with_spaces():
    assert normalize_answer("U.S.A") == normalize_answer("U S A")


def test_an_unknown_grader_name_lists_the_known_names():
    with pytest.raises(ValueError) as caught:
        build_graders(["nope"], None)

    message = str(caught.value)
    assert "nope" in message
    assert "correctness" in message


def test_build_graders_returns_the_named_graders_in_order():
    graders = build_graders(["correctness"], ScriptedJudge("{}"))

    assert [grader.name for grader in graders] == ["correctness"]


def test_a_judge_based_grader_needs_a_judge():
    with pytest.raises(ValueError, match="judge"):
        build_graders(["correctness"], None)


def test_build_graders_rejects_a_name_listed_twice():
    with pytest.raises(ValueError, match="twice"):
        build_graders(["correctness", "correctness"], ScriptedJudge("{}"))


def test_the_registry_lists_correctness_then_the_judge_graders():
    assert default_grader_names() == ("correctness", "faithfulness", "citation_precision")


@pytest.mark.parametrize("status", [GradeStatus.GRADER_ERROR, GradeStatus.NOT_APPLICABLE])
def test_an_unscored_grade_never_carries_a_score(status):
    with pytest.raises(ValueError, match="score"):
        Grade(score=1.0, rationale="why", status=status)

    assert Grade(score=None, rationale="why", status=status).score is None


def test_an_ok_grade_needs_a_score():
    with pytest.raises(ValueError, match="score"):
        Grade(score=None, rationale="why", status=GradeStatus.OK)


def test_faithfulness_scores_the_supported_claim_share():
    judge = ScriptedJudge(
        claims_reply(
            ("The mission launched in 2024.", True),
            ("It carried three satellites.", True),
        )
    )
    item = grading_input(
        "The mission launched in 2024 [1]. It carried three satellites [1].",
        citations=(citation(1, "c1"),),
        retrieved=(retrieved_chunk("c1"),),
    )

    grade = FaithfulnessGrader(judge).grade(item)

    assert grade == Grade(
        score=1.0,
        rationale="2 of 2 claims supported; checked",
        status=GradeStatus.OK,
    )


def test_faithfulness_names_the_first_unsupported_claim():
    judge = ScriptedJudge(
        claims_reply(
            ("The mission launched in 2024.", True),
            ("It reached Mars in 2025.", False),
            ("It carried three satellites.", False),
        )
    )
    item = grading_input(
        "The mission launched in 2024, reached Mars in 2025 and carried three satellites [1].",
        citations=(citation(1, "c1"),),
        retrieved=(retrieved_chunk("c1"),),
    )

    grade = FaithfulnessGrader(judge).grade(item)

    assert grade.status is GradeStatus.OK
    assert grade.score == pytest.approx(1 / 3)
    assert "1 of 3 claims supported" in grade.rationale
    assert "It reached Mars in 2025." in grade.rationale
    assert "checked" in grade.rationale


def test_faithfulness_with_zero_claims_is_not_applicable():
    judge = ScriptedJudge(claims_reply())

    grade = FaithfulnessGrader(judge).grade(
        grading_input(
            "Insufficient information.",
            citations=(citation(1, "c1"),),
            retrieved=(retrieved_chunk("c1"),),
        )
    )

    assert grade == Grade(
        score=None,
        rationale="answer makes no factual claim",
        status=GradeStatus.NOT_APPLICABLE,
    )


def test_an_empty_answer_is_not_applicable_without_calling_the_judge():
    judge = ScriptedJudge(claims_reply(("unused", True)))

    grade = FaithfulnessGrader(judge).grade(grading_input("  \n "))

    assert grade == Grade(
        score=None,
        rationale="empty answer",
        status=GradeStatus.NOT_APPLICABLE,
    )
    assert judge.prompts == []


def test_faithfulness_grades_an_answer_that_cites_nothing():
    judge = ScriptedJudge(claims_reply(("The mission launched in 2024.", False)))

    grade = FaithfulnessGrader(judge).grade(grading_input("The mission launched in 2024."))

    assert grade.status is GradeStatus.OK
    assert grade.score == 0.0
    assert len(judge.prompts) == 1


def test_faithfulness_shows_the_judge_the_cited_chunk_with_its_context_header():
    judge = ScriptedJudge(claims_reply(("The mission launched in 2024.", True)))
    item = grading_input(
        "The mission launched in 2024 [2].",
        citations=(citation(2, "c2"),),
        retrieved=(retrieved_chunk("c2", context_header="# Alpha"),),
    )

    FaithfulnessGrader(judge).grade(item)

    assert "# Alpha\n\nThe Alpha Mission launched in 2024." in judge.prompts[0]


def test_a_cited_chunk_that_was_not_retrieved_is_a_grader_error_naming_the_marker():
    judge = ScriptedJudge(claims_reply(("unused", True)))

    grade = FaithfulnessGrader(judge).grade(
        grading_input(
            "The mission launched in 2024 [3].",
            citations=(citation(3, "missing"),),
            retrieved=(retrieved_chunk("c1"),),
        )
    )

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "[3]" in grade.rationale
    assert judge.prompts == []


@pytest.mark.parametrize(
    "reply",
    [
        "not JSON",
        '{"claims": "nope", "rationale": "wrong type"}',
        '{"claims": [{"claim": "x", "supported": "yes"}], "rationale": "wrong type"}',
        '{"claims": [{"claim": "x"}], "rationale": "missing supported"}',
        '{"rationale": "no claims key"}',
    ],
)
def test_a_wrong_shaped_faithfulness_reply_is_a_grader_error(reply):
    judge = ScriptedJudge(reply)

    grade = FaithfulnessGrader(judge).grade(
        grading_input(
            "The mission launched in 2024 [1].",
            citations=(citation(1, "c1"),),
            retrieved=(retrieved_chunk("c1"),),
        )
    )

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "judge reply" in grade.rationale


def test_a_faithfulness_judge_failure_is_a_grader_error():
    judge = ScriptedJudge(error=JudgeError("judge answered HTTP 503: down"))

    grade = FaithfulnessGrader(judge).grade(
        grading_input(
            "The mission launched in 2024 [1].",
            citations=(citation(1, "c1"),),
            retrieved=(retrieved_chunk("c1"),),
        )
    )

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "HTTP 503" in grade.rationale


def test_faithfulness_ignores_extra_keys_in_the_reply():
    judge = ScriptedJudge(
        '{"claims": [{"claim": "x", "supported": true, "confidence": 0.9}], '
        '"rationale": "checked", "model": "someone"}'
    )
    item = grading_input(
        "The mission launched in 2024 [1].",
        citations=(citation(1, "c1"),),
        retrieved=(retrieved_chunk("c1"),),
    )

    grade = FaithfulnessGrader(judge).grade(item)

    assert grade.status is GradeStatus.OK
    assert grade.score == 1.0


def test_citation_precision_scores_the_supported_marker_share():
    judge = ScriptedJudge(
        citations_reply(
            (1, True),
            (2, True),
        )
    )
    item = grading_input(
        "The mission launched in 2024 [1]. It carried three satellites [2].",
        citations=(citation(1, "c1"), citation(2, "c2")),
        retrieved=(retrieved_chunk("c1"), retrieved_chunk("c2", content="It carried three.")),
    )

    grade = CitationPrecisionGrader(judge).grade(item)

    assert grade.status is GradeStatus.OK
    assert grade.score == 1.0
    assert "2 of 2" in grade.rationale


def test_citation_precision_scores_half_when_one_marker_is_unsupported():
    judge = ScriptedJudge(
        citations_reply(
            (1, True),
            (2, False),
        )
    )
    item = grading_input(
        "The mission launched in 2024 [1]. It reached Mars [2].",
        citations=(citation(1, "c1"), citation(2, "c2")),
        retrieved=(retrieved_chunk("c1"), retrieved_chunk("c2", content="It carried three.")),
    )

    grade = CitationPrecisionGrader(judge).grade(item)

    assert grade.status is GradeStatus.OK
    assert grade.score == 0.5


def test_citation_precision_judges_a_marker_cited_twice_once():
    judge = ScriptedJudge(citations_reply((1, True)))
    item = grading_input(
        "The mission launched in 2024 [1]. It carried three satellites [1].",
        citations=(citation(1, "c1"), citation(1, "c1")),
        retrieved=(retrieved_chunk("c1"),),
    )

    grade = CitationPrecisionGrader(judge).grade(item)

    assert grade.status is GradeStatus.OK
    assert grade.score == 1.0
    assert len(judge.prompts) == 1


def test_an_answer_that_cites_nothing_is_not_applicable_without_calling_the_judge():
    judge = ScriptedJudge(citations_reply((1, True)))

    grade = CitationPrecisionGrader(judge).grade(grading_input("The mission launched in 2024."))

    assert grade == Grade(
        score=None,
        rationale="answer cites no source",
        status=GradeStatus.NOT_APPLICABLE,
    )
    assert judge.prompts == []


@pytest.mark.parametrize(
    "reply",
    [
        citations_reply((1, True)),
        citations_reply((1, True), (2, True), (3, True)),
        citations_reply((1, True), (1, False)),
        "not JSON",
        '{"citations": [{"marker": "1", "supported": true}], "rationale": "wrong type"}',
    ],
)
def test_a_reply_whose_markers_do_not_match_the_answer_is_a_grader_error(reply):
    judge = ScriptedJudge(reply)
    item = grading_input(
        "The mission launched in 2024 [1]. It carried three satellites [2].",
        citations=(citation(1, "c1"), citation(2, "c2")),
        retrieved=(retrieved_chunk("c1"), retrieved_chunk("c2")),
    )

    grade = CitationPrecisionGrader(judge).grade(item)

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "judge reply" in grade.rationale


def test_a_marker_that_names_two_different_chunks_is_a_grader_error():
    judge = ScriptedJudge(citations_reply((1, True)))

    grade = CitationPrecisionGrader(judge).grade(
        grading_input(
            "The mission launched in 2024 [1]. It carried three satellites [1].",
            citations=(citation(1, "c1"), citation(1, "c2")),
            retrieved=(retrieved_chunk("c1"), retrieved_chunk("c2")),
        )
    )

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "[1]" in grade.rationale
    assert "two different Chunks" in grade.rationale
    assert judge.prompts == []


def test_a_cited_chunk_that_was_not_retrieved_is_a_citation_precision_grader_error():
    judge = ScriptedJudge(citations_reply((1, True)))

    grade = CitationPrecisionGrader(judge).grade(
        grading_input(
            "The mission launched in 2024 [1].",
            citations=(citation(1, "missing"),),
            retrieved=(retrieved_chunk("c1"),),
        )
    )

    assert grade.status is GradeStatus.GRADER_ERROR
    assert "[1]" in grade.rationale
    assert judge.prompts == []


def test_a_citation_precision_judge_failure_is_a_grader_error():
    judge = ScriptedJudge(error=httpx.ReadTimeout("judge timed out"))

    grade = CitationPrecisionGrader(judge).grade(
        grading_input(
            "The mission launched in 2024 [1].",
            citations=(citation(1, "c1"),),
            retrieved=(retrieved_chunk("c1"),),
        )
    )

    assert grade.status is GradeStatus.GRADER_ERROR
    assert grade.score is None
    assert "judge" in grade.rationale.lower()
