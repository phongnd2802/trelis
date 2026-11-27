"""Tests for bootstrap confidence intervals and the Ablation Matrix report."""

from __future__ import annotations

import html
import random
from dataclasses import replace
from statistics import mean as statistics_mean

import pytest

from trelis_eval.ablation_run import (
    METRIC_NAMES,
    RECALL_5,
    AblationRun,
    AblationRunFile,
    GoldenSetRef,
    ManifestRef,
    QuestionMetrics,
    QuestionResult,
    QuestionStatus,
    RetrievalConfig,
    RunSummary,
)
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
