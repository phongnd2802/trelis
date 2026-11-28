"""Tests for bootstrap confidence intervals and the Ablation Matrix report."""

from __future__ import annotations

import html
import random
from dataclasses import replace
from pathlib import Path
from statistics import mean as statistics_mean

import pytest

from trelis_eval.ablation_run import (
    METRIC_NAMES,
    RECALL_5,
    AblationRun,
    AblationRunFile,
    GoldenSetRef,
    GradeOutcomes,
    GradeSummary,
    ManifestRef,
    QuestionMetrics,
    QuestionResult,
    QuestionStatus,
    RetrievalConfig,
    RunKind,
    RunSummary,
    load_ablation_run,
)
from trelis_eval.client import Usage
from trelis_eval.graders import CORRECTNESS, Grade, GradeStatus
from trelis_eval.metrics import mean, nearest_rank_percentile
from trelis_eval.report import (
    LATENCY_P50,
    LATENCY_P95,
    bootstrap_ci,
    build_matrix,
    render_html,
    render_markdown,
)

SHA256 = "a" * 64
GOLDEN_PATH = "eval/data/multihop-rag/strat500-golden.jsonl"
NA = "n/a"
PER_QUESTION = (
    (1.0, 1.0, 1.0, 1.0),
    (0.0, 1.0, 0.5, 0.7),
    (0.5, 0.5, 0.0, 0.2),
)
LATENCIES = (10.0, 20.0, 40.0)


def scored_question(index, values, latency_ms):
    return QuestionResult(
        id=f"q{index}",
        slices=("inference_query",),
        status=QuestionStatus.SCORED,
        evidence=("https://example.com/a",),
        retrieved=(),
        trace=(),
        metrics=QuestionMetrics(*values),
        latency_ms=latency_ms,
    )


def unscored_question(index, status):
    return QuestionResult(
        id=f"q{index}",
        slices=("null_query",),
        status=status,
        evidence=(),
        retrieved=(),
        trace=(),
        metrics=None,
        latency_ms=0.0,
        error="boom" if status is QuestionStatus.ERROR else None,
    )


def run_file(
    config,
    per_question=PER_QUESTION,
    latencies=LATENCIES,
    *,
    skipped=0,
    errors=0,
    golden_sha256=SHA256,
    top_k=10,
    run_id="run-1",
    concurrency=1,
):
    """Build one results file from per-question metric values and latencies."""
    questions = [
        scored_question(index, values, latency)
        for index, (values, latency) in enumerate(zip(per_question, latencies, strict=True))
    ]
    questions += [
        unscored_question(len(questions) + index, QuestionStatus.SKIPPED)
        for index in range(skipped)
    ]
    questions += [
        unscored_question(len(questions) + index, QuestionStatus.ERROR) for index in range(errors)
    ]
    summary = RunSummary(
        recall_at_5=mean([values[0] for values in per_question]),
        recall_at_10=mean([values[1] for values in per_question]),
        mrr=mean([values[2] for values in per_question]),
        ndcg_at_10=mean([values[3] for values in per_question]),
        scored=len(per_question),
        skipped=skipped,
        error=errors,
        latency_p50=nearest_rank_percentile(latencies, 50),
        latency_p95=nearest_rank_percentile(latencies, 95),
    )
    return AblationRunFile(
        run_id=run_id,
        golden_set=GoldenSetRef(
            path=GOLDEN_PATH,
            sha256=golden_sha256,
            questions=len(questions),
        ),
        manifest=ManifestRef(
            path="eval/data/multihop-rag/strat500-manifest.jsonl",
            sha256="b" * 64,
        ),
        base_url="http://localhost:8080",
        harness_version="0.1.0",
        run=AblationRun(
            config=RetrievalConfig(name=config, techniques=(config,)),
            top_k=top_k,
            started_at="2026-01-01T00:00:00+00:00",
            finished_at="2026-01-01T00:01:00+00:00",
            questions=tuple(questions),
            summary=summary,
            concurrency=concurrency,
        ),
    )


# Per-question values whose bootstrap means take many distinct values, so two
# seeds give different bounds.
VARIED_VALUES = [0.0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.55, 0.6, 0.7, 0.8, 0.9, 0.27]


def grade_for_score(score):
    if score is None:
        return Grade(
            score=None,
            rationale="judge failed",
            status=GradeStatus.GRADER_ERROR,
        )
    if score == NA:
        return Grade(
            score=None,
            rationale="empty answer",
            status=GradeStatus.NOT_APPLICABLE,
        )
    return Grade(score=score, rationale="graded", status=GradeStatus.OK)


def answer_question(index, score, latency_ms=10.0):
    return QuestionResult(
        id=f"a{index}",
        slices=("inference_query",),
        status=QuestionStatus.SCORED,
        evidence=("https://example.com/a",),
        retrieved=(),
        trace=(),
        metrics=QuestionMetrics(1.0, 1.0, 1.0, 1.0),
        latency_ms=latency_ms,
        answer="An answer.",
        finish_reason="stop",
        usage=Usage(prompt_tokens=1, completion_tokens=1, total_tokens=2),
        grades={"correctness": grade_for_score(score)},
    )


def answer_run_file(config, correctness, *, judge_model="judge-model", run_id="answer-1"):
    """Build one answer results file with one correctness Grade per value.

    ``None`` in ``correctness`` is a grader_error; a float is an ok score.
    """
    questions = [answer_question(index, score) for index, score in enumerate(correctness)]
    ok_scores = [score for score in correctness if score is not None]
    summary = RunSummary(
        recall_at_5=mean([1.0 for _ in questions]),
        recall_at_10=mean([1.0 for _ in questions]),
        mrr=mean([1.0 for _ in questions]),
        ndcg_at_10=mean([1.0 for _ in questions]),
        scored=len(questions),
        skipped=0,
        error=0,
        latency_p50=nearest_rank_percentile([10.0 for _ in questions], 50),
        latency_p95=nearest_rank_percentile([10.0 for _ in questions], 95),
        grades={
            "correctness": GradeSummary(
                mean=mean(ok_scores),
                ok=len(ok_scores),
                grader_error=sum(1 for score in correctness if score is None),
            )
        },
    )
    return AblationRunFile(
        run_id=run_id,
        golden_set=GoldenSetRef(
            path=GOLDEN_PATH,
            sha256=SHA256,
            questions=len(questions),
        ),
        manifest=ManifestRef(
            path="eval/data/multihop-rag/strat500-manifest.jsonl",
            sha256="b" * 64,
        ),
        base_url="http://localhost:8080",
        harness_version="0.1.0",
        run=AblationRun(
            config=RetrievalConfig(name=config, techniques=(config,)),
            top_k=10,
            started_at="2026-01-01T00:00:00+00:00",
            finished_at="2026-01-01T00:01:00+00:00",
            questions=tuple(questions),
            summary=summary,
            kind=RunKind.ANSWER,
            judge_model=judge_model,
            graders=("correctness",),
        ),
    )


def multi_grader_answer_run_file(config, grades_by_name, *, run_id):
    """Build one answer results file with one Grade per named Grader and question."""
    count = len(next(iter(grades_by_name.values())))
    questions = []
    for index in range(count):
        grades = {name: grade_for_score(values[index]) for name, values in grades_by_name.items()}
        questions.append(replace(answer_question(index, 1.0), grades=grades))
    summary_grades = {}
    for name, values in grades_by_name.items():
        ok_scores = [value for value in values if isinstance(value, (int, float))]
        summary_grades[name] = GradeSummary(
            mean=mean(ok_scores),
            ok=len(ok_scores),
            grader_error=sum(1 for value in values if value is None),
            not_applicable=sum(1 for value in values if value == NA),
        )
    summary = RunSummary(
        recall_at_5=1.0,
        recall_at_10=1.0,
        mrr=1.0,
        ndcg_at_10=1.0,
        scored=count,
        skipped=0,
        error=0,
        latency_p50=nearest_rank_percentile([10.0 for _ in questions], 50),
        latency_p95=nearest_rank_percentile([10.0 for _ in questions], 95),
        grades=summary_grades,
    )
    return AblationRunFile(
        run_id=run_id,
        golden_set=GoldenSetRef(path=GOLDEN_PATH, sha256=SHA256, questions=count),
        manifest=ManifestRef(
            path="eval/data/multihop-rag/strat500-manifest.jsonl",
            sha256="b" * 64,
        ),
        base_url="http://localhost:8080",
        harness_version="0.1.0",
        run=AblationRun(
            config=RetrievalConfig(name=config, techniques=(config,)),
            top_k=10,
            started_at="2026-01-01T00:00:00+00:00",
            finished_at="2026-01-01T00:01:00+00:00",
            questions=tuple(questions),
            summary=summary,
            kind=RunKind.ANSWER,
            judge_model="judge-model",
            graders=tuple(grades_by_name),
        ),
    )


def test_correctness_uses_only_ok_grades_and_shows_the_error_count():
    run = answer_run_file("dense", [1.0, 0.0, 1.0, None])
    scores = [1.0, 0.0, 1.0]

    matrix = build_matrix([run], seed=3, resamples=200)

    [row] = matrix.rows
    assert row.kind is RunKind.ANSWER
    assert row.grader_outcomes["correctness"].grader_error == 1
    estimate = row.estimates[CORRECTNESS]
    assert estimate is not None
    assert estimate.point == pytest.approx(2 / 3)
    expected = bootstrap_ci(
        scores,
        statistics_mean,
        rng=random.Random("3:dense:correctness"),
        resamples=200,
    )
    assert expected is not None
    assert (estimate.low, estimate.high) == expected
    table_row = next(
        line for line in render_markdown(matrix).splitlines() if line.startswith("| dense |")
    )
    assert f"0.667 [{expected[0]:.3f}, {expected[1]:.3f}] · 1 error" in table_row


def test_an_answer_row_with_only_grader_errors_shows_a_dash_and_the_count():
    run = answer_run_file("dense", [None, None, None])

    matrix = build_matrix([run], seed=1, resamples=50)

    [row] = matrix.rows
    assert row.estimates[CORRECTNESS] is None
    assert row.grader_outcomes["correctness"].grader_error == 3
    table_row = next(
        line for line in render_markdown(matrix).splitlines() if line.startswith("| dense |")
    )
    assert "— · 3 errors" in table_row


def test_an_answer_run_with_all_three_graders_gets_three_columns_in_registry_order():
    run = multi_grader_answer_run_file(
        "dense",
        {
            "correctness": [1.0, 0.0],
            "faithfulness": [0.5, 0.5],
            "citation_precision": [1.0, 0.5],
        },
        run_id="all-three",
    )

    matrix = build_matrix([run], seed=1, resamples=20)

    assert matrix.graders == ("correctness", "faithfulness", "citation_precision")
    header = next(
        line
        for line in render_markdown(matrix).splitlines()
        if line.startswith("| Retrieval Config")
    )
    assert header.split(" | ")[5:8] == [
        "Correctness",
        "Faithfulness",
        "Citation precision",
    ]


def test_a_grader_cell_shows_the_error_and_n_a_parts_only_when_non_zero():
    run = multi_grader_answer_run_file(
        "dense",
        {"faithfulness": [1.0, 0.0, None, NA, NA]},
        run_id="counts",
    )

    matrix = build_matrix([run], seed=1, resamples=20)
    [row] = matrix.rows
    table_row = next(
        line for line in render_markdown(matrix).splitlines() if line.startswith("| dense |")
    )

    assert row.grader_outcomes["faithfulness"].grader_error == 1
    assert row.grader_outcomes["faithfulness"].not_applicable == 2
    assert "0.500 [" in table_row
    assert "· 1 error · 2 n/a" in table_row


def test_a_grader_cell_with_only_not_applicable_grades_shows_a_dash():
    run = multi_grader_answer_run_file(
        "dense",
        {"faithfulness": [NA, NA, NA]},
        run_id="only-n-a",
    )

    matrix = build_matrix([run], seed=1, resamples=20)
    [row] = matrix.rows
    table_row = next(
        line for line in render_markdown(matrix).splitlines() if line.startswith("| dense |")
    )

    assert row.estimates["faithfulness"] is None
    assert row.grader_outcomes["faithfulness"].not_applicable == 3
    assert "— · 3 n/a" in table_row


def test_a_grader_cell_with_no_failures_has_no_trailing_counts():
    run = multi_grader_answer_run_file(
        "dense",
        {"faithfulness": [1.0, 1.0]},
        run_id="no-counts",
    )

    table_row = next(
        line
        for line in render_markdown(build_matrix([run], seed=1, resamples=20)).splitlines()
        if line.startswith("| dense |")
    )

    cell = table_row.split(" | ")[5]
    assert "[" in cell
    assert "·" not in cell


def test_not_applicable_grades_are_outside_the_mean_and_the_interval():
    run = multi_grader_answer_run_file(
        "dense",
        {"faithfulness": [1.0, 0.0, NA, NA]},
        run_id="na-outside",
    )

    matrix = build_matrix([run], seed=3, resamples=200)

    [row] = matrix.rows
    estimate = row.estimates["faithfulness"]
    assert estimate is not None
    assert estimate.point == pytest.approx(0.5)
    expected = bootstrap_ci(
        [1.0, 0.0],
        statistics_mean,
        rng=random.Random("3:dense:faithfulness"),
        resamples=200,
    )
    assert expected is not None
    assert (estimate.low, estimate.high) == expected
    assert row.grader_outcomes["faithfulness"].grader_error == 0
    assert row.grader_outcomes["faithfulness"].not_applicable == 2


def test_a_retrieval_row_shows_a_dash_in_the_correctness_column():
    matrix = build_matrix(
        [run_file("bm25"), answer_run_file("dense", [1.0])],
        seed=1,
        resamples=50,
    )

    retrieval_row = matrix.rows[0]
    assert retrieval_row.kind is RunKind.RETRIEVAL
    assert retrieval_row.graders == frozenset()
    assert retrieval_row.estimates[CORRECTNESS] is None
    assert retrieval_row.grader_outcomes == {}
    table_row = next(
        line for line in render_markdown(matrix).splitlines() if line.startswith("| bm25 |")
    )
    # The cell after nDCG@10 is the correctness column.
    assert table_row.split(" | ")[5] == "—"


def test_a_report_mixes_a_retrieval_row_and_an_answer_row():
    matrix = build_matrix(
        [run_file("bm25"), answer_run_file("dense", [1.0, 0.0])],
        seed=1,
        resamples=50,
    )

    markdown = render_markdown(matrix)
    retrieval_row = next(line for line in markdown.splitlines() if line.startswith("| bm25 |"))
    answer_row = next(line for line in markdown.splitlines() if line.startswith("| dense |"))

    assert retrieval_row.split(" | ")[5] == "—"
    assert "0.500 [" in answer_row


def test_one_column_per_grader_in_registry_order_then_sorted_unknown_names():
    full = multi_grader_answer_run_file(
        "dense",
        {
            "correctness": [1.0, 0.0],
            "zeta": [0.5, 0.5],
            "alpha": [0.0, 1.0],
        },
        run_id="multi-full",
    )
    partial = multi_grader_answer_run_file(
        "bm25", {"correctness": [1.0, 1.0]}, run_id="multi-partial"
    )

    matrix = build_matrix([full, partial], seed=1, resamples=20)

    assert matrix.graders == ("correctness", "alpha", "zeta")
    lines = render_markdown(matrix).splitlines()
    header = next(line for line in lines if line.startswith("| Retrieval Config"))
    assert header.split(" | ")[5:8] == ["Correctness", "alpha", "zeta"]
    full_cells = next(line for line in lines if line.startswith("| dense |")).split(" | ")
    partial_cells = next(line for line in lines if line.startswith("| bm25 |")).split(" | ")
    assert "0.500 [" in full_cells[6]
    assert "0.500 [" in full_cells[7]
    assert partial_cells[6] == "—"
    assert partial_cells[7] == "—"
    assert matrix.rows[0].grader_outcomes == {
        "correctness": GradeOutcomes(scores=(1.0, 0.0), grader_error=0, not_applicable=0),
        "alpha": GradeOutcomes(scores=(0.0, 1.0), grader_error=0, not_applicable=0),
        "zeta": GradeOutcomes(scores=(0.5, 0.5), grader_error=0, not_applicable=0),
    }
    assert matrix.rows[1].grader_outcomes == {
        "correctness": GradeOutcomes(scores=(1.0, 1.0), grader_error=0, not_applicable=0)
    }
    assert matrix.rows[1].graders == frozenset({"correctness"})


def test_the_committed_version_1_baselines_still_render():
    baselines = sorted((Path(__file__).parent.parent / "baselines").glob("*.jsonl"))
    assert baselines

    matrix = build_matrix(
        [load_ablation_run(path) for path in baselines],
        seed=1,
        resamples=50,
    )

    assert all(row.kind is RunKind.RETRIEVAL for row in matrix.rows)
    assert matrix.graders == ()
    assert all(row.graders == frozenset() for row in matrix.rows)
    assert all(row.estimates.get(CORRECTNESS) is None for row in matrix.rows)
    markdown = render_markdown(matrix)
    assert "Correctness" not in markdown
    assert "<table>" in render_html(matrix)


class FixedSampler(random.Random):
    """A Random that returns prepared resamples instead of drawing them."""

    def __init__(self, samples):
        super().__init__(0)
        self.samples = list(samples)

    def choices(self, population, *, k):
        assert k == len(population)
        return self.samples.pop(0)


def test_bootstrap_ci_is_reproducible_and_holds_the_sample_mean():
    values = [float(value) for value in range(1, 11)]

    first = bootstrap_ci(values, statistics_mean, rng=random.Random(4), resamples=1000)
    second = bootstrap_ci(values, statistics_mean, rng=random.Random(4), resamples=1000)

    assert first == second
    assert first is not None
    low, high = first
    assert low < high
    assert low <= statistics_mean(values) <= high


def test_bootstrap_ci_changes_with_the_seed():
    values = VARIED_VALUES

    first = bootstrap_ci(values, statistics_mean, rng=random.Random(1), resamples=1000)
    second = bootstrap_ci(values, statistics_mean, rng=random.Random(2), resamples=1000)

    assert first != second


def test_bootstrap_ci_is_none_without_values_and_the_value_itself_for_constants():
    assert bootstrap_ci([], statistics_mean, rng=random.Random(0), resamples=100) is None
    assert bootstrap_ci(
        [0.25, 0.25, 0.25], statistics_mean, rng=random.Random(0), resamples=100
    ) == (0.25, 0.25)


def test_bootstrap_ci_takes_the_nearest_rank_bounds():
    # Four resamples: ranks ceil(0.025 * 4) = 1 and ceil(0.975 * 4) = 4.
    rng = FixedSampler([[40.0, 40.0], [10.0, 10.0], [30.0, 30.0], [20.0, 20.0]])

    assert bootstrap_ci([0.0, 1.0], lambda values: values[0], rng=rng, resamples=4) == (
        10.0,
        40.0,
    )
    assert rng.samples == []

    # Forty resamples: the upper bound is rank ceil(0.975 * 40) = 39, not 40.
    rng = FixedSampler([[float(value)] * 2 for value in range(40, 0, -1)])

    assert bootstrap_ci([0.0, 1.0], lambda values: values[0], rng=rng, resamples=40) == (
        1.0,
        39.0,
    )


def test_bootstrap_ci_rejects_a_resample_count_below_one():
    with pytest.raises(ValueError, match="resamples"):
        bootstrap_ci([1.0, 2.0], statistics_mean, rng=random.Random(0), resamples=0)


@pytest.mark.parametrize("confidence", [0.0, 1.0, -0.5, 1.5])
def test_bootstrap_ci_rejects_a_confidence_outside_zero_and_one(confidence):
    with pytest.raises(ValueError, match="confidence"):
        bootstrap_ci(
            [1.0, 2.0],
            statistics_mean,
            rng=random.Random(0),
            resamples=10,
            confidence=confidence,
        )


def test_a_lower_confidence_level_gives_a_narrower_interval():
    values = [float(value) for value in range(1, 11)]
    narrower = bootstrap_ci(
        values, statistics_mean, rng=random.Random(5), resamples=2000, confidence=0.8
    )
    wider = bootstrap_ci(
        values, statistics_mean, rng=random.Random(5), resamples=2000, confidence=0.95
    )

    assert narrower is not None
    assert wider is not None
    assert narrower[1] - narrower[0] < wider[1] - wider[0]


def test_matrix_point_estimates_are_the_summary_means_and_every_metric_has_an_interval():
    run = run_file("dense")

    matrix = build_matrix([run], seed=3, resamples=200)

    assert matrix.golden_set == run.golden_set
    assert matrix.top_k == 10
    assert matrix.seed == 3
    assert matrix.resamples == 200
    assert matrix.confidence == 0.95
    [row] = matrix.rows
    assert row.config == "dense"
    assert row.run_id == "run-1"
    assert row.harness_version == "0.1.0"
    assert row.concurrency == 1
    assert (row.scored, row.skipped, row.error) == (3, 0, 0)
    summary = run.run.summary
    recorded = (
        summary.recall_at_5,
        summary.recall_at_10,
        summary.mrr,
        summary.ndcg_at_10,
    )
    for name, point in zip(METRIC_NAMES, recorded, strict=True):
        estimate = row.estimates[name]
        assert estimate is not None
        assert estimate.point == point
        assert estimate.low <= estimate.high
    latency = row.estimates[LATENCY_P50]
    assert latency is not None
    assert latency.point == summary.latency_p50
    latency = row.estimates[LATENCY_P95]
    assert latency is not None
    assert latency.point == summary.latency_p95


def test_matrix_shows_n_a_for_every_metric_when_nothing_was_scored():
    run = run_file("dense", (), (), skipped=2)

    matrix = build_matrix([run], seed=1, resamples=50)

    [row] = matrix.rows
    assert (row.scored, row.skipped, row.error) == (0, 2, 0)
    assert all(estimate is None for estimate in row.estimates.values())
    assert row.estimates[LATENCY_P50] is None
    assert row.estimates[LATENCY_P95] is None
    markdown = render_markdown(matrix)
    assert "| n/a |" in markdown
    assert "0.000" not in markdown
    page = render_html(matrix)
    assert '<td class="num">n/a</td>' in page
    assert "0.000" not in page


def test_matrix_rejects_results_from_different_golden_sets():
    first = run_file("dense")
    second = run_file("bm25", golden_sha256="c" * 64)

    with pytest.raises(ValueError, match="Golden Set"):
        build_matrix([first, second], seed=1, resamples=50)


def test_matrix_rejects_results_with_different_top_k():
    first = run_file("dense")
    second = run_file("bm25", top_k=20)

    with pytest.raises(ValueError, match="top_k"):
        build_matrix([first, second], seed=1, resamples=50)


def test_matrix_rejects_two_files_for_the_same_retrieval_config():
    first = run_file("dense")
    second = run_file("dense", run_id="run-2")

    with pytest.raises(ValueError, match="dense"):
        build_matrix([first, second], seed=1, resamples=50)


def test_matrix_rejects_no_results_files():
    with pytest.raises(ValueError, match="no results"):
        build_matrix([], seed=1, resamples=50)


def test_reordering_files_does_not_change_a_rows_interval():
    dense = run_file("dense")
    bm25 = run_file("bm25", latencies=(5.0, 15.0, 25.0))
    hybrid = run_file("bm25+dense", tuple(reversed(PER_QUESTION)), (7.0, 11.0, 13.0))

    forward = build_matrix([dense, bm25, hybrid], seed=11, resamples=300)
    backward = build_matrix([hybrid, bm25, dense], seed=11, resamples=300)

    assert forward.rows == tuple(reversed(backward.rows))


def test_render_markdown_puts_an_interval_in_every_metric_and_latency_cell():
    matrix = build_matrix([run_file("dense")], seed=2, resamples=100)

    markdown = render_markdown(matrix)

    [row] = matrix.rows
    table_rows = [line for line in markdown.splitlines() if line.startswith("| dense |")]
    assert table_rows[0].count("[") == 6
    assert table_rows[0].count("]") == 6
    for header in (
        "Recall@5",
        "Recall@10",
        "MRR",
        "nDCG@10",
        "Latency p50 (ms)",
        "Latency p95 (ms)",
        "Concurrency",
    ):
        assert header in markdown
    assert "100 resamples" in markdown
    assert "95%" in markdown
    assert "seed 2" in markdown
    assert matrix.golden_set.sha256 in markdown
    assert "top_k: 10" in markdown
    assert "run-1" in markdown
    assert "0.1.0" in markdown
    latency = row.estimates[LATENCY_P50]
    assert latency is not None
    assert f"{latency.point:.1f} [{latency.low:.1f}, {latency.high:.1f}]" in table_rows[0]


def test_render_html_escapes_config_names_and_paths_and_references_no_external_assets():
    name = '<script>alert("x")</script>'
    path = '<b>"golden"</b>'
    source = run_file(name)
    run = replace(source, golden_set=replace(source.golden_set, path=path))
    matrix = build_matrix([run], seed=1, resamples=50)

    page = render_html(matrix)

    assert "<script" not in page.lower()
    assert "<link" not in page.lower()
    assert " src=" not in page.lower()
    assert html.escape(name) in page
    assert html.escape(path) in page
    estimate = matrix.rows[0].estimates[RECALL_5]
    assert estimate is not None
    assert f"{estimate.point:.3f} [{estimate.low:.3f}, {estimate.high:.3f}]" in page


def test_render_html_marks_a_report_mixing_concurrencies():
    matrix = build_matrix(
        [run_file("dense", concurrency=1), run_file("bm25", concurrency=4)],
        seed=1,
        resamples=50,
    )

    page = render_html(matrix)
    markdown = render_markdown(matrix)

    assert "different concurrency" in page.lower()
    assert "different concurrency" in markdown.lower()
