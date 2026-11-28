"""Tests for the Grader registry and the correctness Grader."""

import httpx
import pytest
from fakes import ScriptedJudge

from trelis_eval.graders import (
    CorrectnessGrader,
    Grade,
    GradeStatus,
    GradingInput,
    build_graders,
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


def test_an_empty_expected_answer_is_a_grader_error_without_calling_the_judge():
    judge = ScriptedJudge('{"correct": true, "rationale": "unused"}')

    grade = CorrectnessGrader(judge).grade(grading_input("Paris", expected_answer="  "))

    assert grade == Grade(
        score=None,
        rationale="no expected answer",
        status=GradeStatus.GRADER_ERROR,
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
