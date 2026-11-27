"""Tests for the Golden Set JSONL format and its validation."""

import json

import pytest

from trelis_eval.golden import GoldenQuestion, GoldenSetError, load_golden_set, write_golden_set
from trelis_eval.golden import Evidence


def write_lines(tmp_path, *lines):
    path = tmp_path / "golden.jsonl"
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


def valid_line(**overrides):
    line = {
        "id": "q1",
        "question": "Who founded Trelis?",
        "expected_answer": "Nobody yet.",
        "evidence": [{"source_id": "https://example.com/a", "fact": "It is a project."}],
        "slices": ["comparison_query", "hops-1"],
        "principal": "alice",
    }
    line.update(overrides)
    return json.dumps(line)


def test_valid_file_loads_all_fields(tmp_path):
    path = write_lines(tmp_path, valid_line())

    [question] = load_golden_set(path)

    assert question == GoldenQuestion(
        id="q1",
        question="Who founded Trelis?",
        expected_answer="Nobody yet.",
        evidence=(Evidence(source_id="https://example.com/a", fact="It is a project."),),
        slices=("comparison_query", "hops-1"),
        principal="alice",
    )


def test_evidence_may_be_empty_and_principal_may_be_absent(tmp_path):
    path = write_lines(
        tmp_path,
        json.dumps(
            {
                "id": "q1",
                "question": "Who?",
                "expected_answer": "Insufficient information.",
                "evidence": [],
                "slices": ["hops-0"],
            }
        ),
    )

    [question] = load_golden_set(path)

    assert question.evidence == ()
    assert question.principal is None


@pytest.mark.parametrize(
    "line",
    [
        "not json",
        "[]",
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": "A", "evidence": []}),
        json.dumps({"id": 1, "question": "Who?", "expected_answer": "A", "evidence": [], "slices": []}),
        json.dumps({"id": "", "question": "Who?", "expected_answer": "A", "evidence": [], "slices": []}),
        json.dumps({"id": "q1", "question": "  ", "expected_answer": "A", "evidence": [], "slices": []}),
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": 7, "evidence": [], "slices": []}),
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": "A", "evidence": {}, "slices": []}),
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": "A", "evidence": ["x"], "slices": []}),
        json.dumps(
            {
                "id": "q1",
                "question": "Who?",
                "expected_answer": "A",
                "evidence": [{"source_id": "u"}],
                "slices": [],
            }
        ),
        json.dumps(
            {
                "id": "q1",
                "question": "Who?",
                "expected_answer": "A",
                "evidence": [{"source_id": "", "fact": "f"}],
                "slices": [],
            }
        ),
        json.dumps(
            {
                "id": "q1",
                "question": "Who?",
                "expected_answer": "A",
                "evidence": [{"source_id": "u", "fact": "f", "extra": 1}],
                "slices": [],
            }
        ),
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": "A", "evidence": [], "slices": "hops-1"}),
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": "A", "evidence": [], "slices": [""]}),
        json.dumps({"id": "q1", "question": "Who?", "expected_answer": "A", "evidence": [], "slices": [1]}),
        json.dumps(
            {
                "id": "q1",
                "question": "Who?",
                "expected_answer": "A",
                "evidence": [],
                "slices": [],
                "unknown": True,
            }
        ),
        json.dumps(
            {
                "id": "q1",
                "question": "Who?",
                "expected_answer": "A",
                "evidence": [],
                "slices": [],
                "principal": 7,
            }
        ),
        "",
    ],
)
def test_each_validation_rule_rejects_with_a_line_number(tmp_path, line):
    path = write_lines(tmp_path, line)

    with pytest.raises(GoldenSetError) as caught:
        load_golden_set(path)

    assert f"{path}:1: " in str(caught.value)


def test_duplicate_ids_are_rejected(tmp_path):
    path = write_lines(tmp_path, valid_line(), valid_line())

    with pytest.raises(GoldenSetError, match=r":2: duplicate id 'q1'"):
        load_golden_set(path)


def test_all_problems_are_reported_together(tmp_path):
    bad_field = json.dumps({"id": "q2", "question": "Who?", "expected_answer": "A", "evidence": []})
    path = write_lines(tmp_path, "not json", bad_field)

    with pytest.raises(GoldenSetError) as caught:
        load_golden_set(path)

    message = str(caught.value)
    assert f"{path}:1: invalid JSON" in message
    assert f"{path}:2: missing field 'slices'" in message


def test_write_then_load_is_lossless(tmp_path):
    questions = [
        GoldenQuestion(
            id="q1",
            question="Ai là người sáng lập?",
            expected_answer="Không rõ.",
            evidence=(
                Evidence(source_id="https://example.com/a", fact="Một dự án."),
                Evidence(source_id="https://example.com/b", fact="Hai dòng."),
            ),
            slices=("comparison_query", "hops-2"),
            principal="alice",
        ),
        GoldenQuestion(
            id="q2",
            question="When?",
            expected_answer="",
            evidence=(),
            slices=("null_query", "hops-0"),
        ),
    ]
    path = tmp_path / "round-trip.jsonl"

    write_golden_set(path, questions)

    assert load_golden_set(path) == questions
    lines = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
    assert "principal" not in lines[1]
    assert lines[0]["question"] == "Ai là người sáng lập?"