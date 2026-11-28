"""The Ablation Matrix report: bootstrap confidence intervals over results files.

The report is built from Ablation Run results files alone. Every Retrieval
Config gets a row with the four metric means, one column per Grader in the
report, the two latency percentiles (shown only when the run measured the
provider), Tokens and Cost per Correct Answer from a configured price table,
each with a seeded percentile-bootstrap confidence interval, plus the counts
and the run's concurrency. Each Slice then gets a table of the same metric and
Grader estimates, and each answer run gets a Failure Attribution table that
labels its scored questions by the stage that failed. The same files, seed and
price table render byte-identical Markdown and HTML: no wall-clock time, no
machine paths and no randomness outside the seeded RNG.
``eval/docs/metrics.md`` holds the definitions.
"""

from __future__ import annotations

import math
import random
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from fractions import Fraction
from html import escape
from typing import Any, cast

from .ablation_run import (
    METRIC_NAMES,
    MRR,
    NDCG_10,
    RECALL_5,
    RECALL_10,
    AblationRun,
    AblationRunFile,
    GoldenSetRef,
    GradeOutcomes,
    ModelMode,
    QuestionResult,
    QuestionStatus,
    RunKind,
    grade_outcomes,
)
from .attribution import FailureLabel, attribute_failure
from .golden import hop_count
from .graders import CORRECTNESS, GradeStatus, default_grader_names, grader_label
from .metrics import mean, nearest_rank_percentile
from .prices import PriceTable, generate_model, question_cost

LATENCY_P50 = "latency_p50"
LATENCY_P95 = "latency_p95"
TOKENS = "tokens"
"""The estimate key of the mean total tokens per scored answer question."""
COST_PER_CORRECT = "cost_per_correct"
"""The estimate key of mean cost divided by mean correctness score."""

_MEASURED_MODEL_MODES = frozenset({ModelMode.LIVE, ModelMode.PASSTHROUGH})

METRIC_LABELS = {
    RECALL_5: "Recall@5",
    RECALL_10: "Recall@10",
    MRR: "MRR",
    NDCG_10: "nDCG@10",
}


@dataclass(frozen=True)
class MetricEstimate:
    """A point estimate with its percentile-bootstrap confidence interval."""

    point: float
    low: float
    high: float


@dataclass(frozen=True)
class CostObservations:
    """What one row's Cost per Correct Answer can be built from.

    ``pairs`` holds one ``(cost, correctness score)`` per priced question with
    an ok correctness Grade, ``unpriced_models`` names the generate models the
    price table lacks, and ``questions_without_model`` counts the ok-graded
    answers whose trace has no generate step. An unpriced model or a missing
    model makes the whole row's cost unavailable.
    """

    pairs: tuple[tuple[float, float], ...] = ()
    unpriced_models: tuple[str, ...] = ()
    questions_without_model: int = 0


@dataclass(frozen=True)
class MatrixRow:
    """One Retrieval Config's Ablation Run, with every estimate's interval."""

    config: str
    run_id: str
    harness_version: str
    concurrency: int
    kind: RunKind
    model_mode: ModelMode
    scored: int
    skipped: int
    error: int
    graders: frozenset[str]
    grader_outcomes: dict[str, GradeOutcomes]
    estimates: dict[str, MetricEstimate | None]
    cost: CostObservations = CostObservations()


@dataclass(frozen=True)
class SliceRow:
    """One Retrieval Config's estimates over one Slice of its Ablation Run."""

    config: str
    questions: int
    scored: int
    graders: frozenset[str]
    grader_outcomes: dict[str, GradeOutcomes]
    estimates: dict[str, MetricEstimate | None]


@dataclass(frozen=True)
class SliceTable:
    """One Slice's table: its Golden Set question count and one row per config."""

    name: str
    questions: int
    rows: tuple[SliceRow, ...]


@dataclass(frozen=True)
class AttributionRow:
    """One Failure Attribution row: ``All`` or one Slice, with its label counts."""

    name: str
    graded: int
    counts: dict[FailureLabel, int]


@dataclass(frozen=True)
class AttributionTable:
    """One answer Ablation Run's Failure Attribution, one row per Slice."""

    config: str
    rows: tuple[AttributionRow, ...]


@dataclass(frozen=True)
class AblationMatrix:
    """One row per Retrieval Config, with the metadata the rows share."""

    golden_set: GoldenSetRef
    top_k: int
    seed: int
    resamples: int
    confidence: float
    graders: tuple[str, ...]
    rows: tuple[MatrixRow, ...]
    slice_tables: tuple[SliceTable, ...]
    attribution_tables: tuple[AttributionTable, ...]
    currency: str | None = None


def bootstrap_ci[T](
    values: Sequence[T],
    statistic: Callable[[Sequence[T]], float],
    *,
    rng: random.Random,
    resamples: int,
    confidence: float = 0.95,
) -> tuple[float, float] | None:
    """Return the percentile-bootstrap interval of ``statistic`` over ``values``.

    ``resamples`` resamples of ``values``' length are drawn with replacement
    from ``rng``, the statistic is computed on each, and the bounds are the
    sorted values at rank ``ceil(q * resamples)`` (1-based, at least 1) for
    ``q = (1 - confidence) / 2`` and its complement. No values gives ``None``;
    constant values give their statistic at both bounds.
    """
    _check_resamples(resamples)
    _check_confidence(confidence)
    if not values:
        return None
    if len(set(values)) == 1:
        fixed = statistic(values)
        return fixed, fixed

    lower_rank, upper_rank = _bound_ranks(resamples, confidence)
    estimates = []
    for _ in range(resamples):
        sample = rng.choices(values, k=len(values))
        estimates.append(statistic(sample))
    estimates.sort()
    return estimates[lower_rank - 1], estimates[upper_rank - 1]


def build_matrix(
    run_files: Sequence[AblationRunFile],
    *,
    seed: int,
    resamples: int,
    confidence: float = 0.95,
    prices: PriceTable | None = None,
) -> AblationMatrix:
    """Build the Ablation Matrix from loaded Ablation Run results files.

    Rows keep the order of ``run_files``. A metric's point estimate is the mean
    over its scored questions, so it equals the results file's summary, and a
    latency percentile uses the same nearest-rank rule. Each interval draws
    from its own RNG keyed by the seed, config name and metric, so reordering
    rows cannot change another row's interval. ``prices`` prices Cost per
    Correct Answer; without it the column is unavailable.
    """
    _check_resamples(resamples)
    _check_confidence(confidence)
    if not run_files:
        raise ValueError("no results files to report on")
    _check_comparable(run_files)
    grader_names = _report_grader_names(run_files)
    slice_names = _slice_names(run_files)
    slice_question_counts = _slice_question_counts(run_files, slice_names)

    rows = []
    slice_rows: dict[str, list[SliceRow]] = {name: [] for name in slice_names}
    attribution_tables = []
    for run_file in run_files:
        run = run_file.run
        config = run.config.name
        run_graders = _graders_in_run(run)
        metric_values, latencies, grader_outcomes = _observations(
            run.questions, grader_names, run_graders
        )
        series: list[_Series] = [
            *_mean_series(metric_values, grader_outcomes),
            (LATENCY_P50, latencies, _resample_percentile(50)),
            (LATENCY_P95, latencies, _resample_percentile(95)),
        ]
        cost = _cost_observations(run, prices)
        if run.kind is RunKind.ANSWER:
            series.append((TOKENS, _token_values(run), _resample_mean))
            if prices is not None and not cost.unpriced_models and not cost.questions_without_model:
                series.append((COST_PER_CORRECT, cost.pairs, _resample_cost_per_correct))
        estimates = _row_estimates(
            series,
            grader_names,
            key_parts=(str(seed), config),
            resamples=resamples,
            confidence=confidence,
        )
        rows.append(
            MatrixRow(
                config=config,
                run_id=run_file.run_id,
                harness_version=run_file.harness_version,
                concurrency=run.concurrency,
                kind=run.kind,
                model_mode=run.model_mode,
                scored=run.summary.scored,
                skipped=run.summary.skipped,
                error=run.summary.error,
                graders=run_graders,
                grader_outcomes=grader_outcomes,
                estimates=estimates,
                cost=cost,
            )
        )

        for name in slice_names:
            questions = [result for result in run.questions if name in result.slices]
            scored = sum(1 for result in questions if result.status is QuestionStatus.SCORED)
            slice_metric_values, _, slice_outcomes = _observations(
                questions, grader_names, run_graders
            )
            slice_rows[name].append(
                SliceRow(
                    config=config,
                    questions=len(questions),
                    scored=scored,
                    graders=run_graders,
                    grader_outcomes=slice_outcomes,
                    estimates=_row_estimates(
                        _mean_series(slice_metric_values, slice_outcomes),
                        grader_names,
                        key_parts=(str(seed), config, name),
                        resamples=resamples,
                        confidence=confidence,
                    ),
                )
            )

        if run.kind is RunKind.ANSWER:
            attribution_tables.append(_attribution_table(config, run.questions, slice_names))

    first = run_files[0]
    return AblationMatrix(
        golden_set=first.golden_set,
        top_k=first.run.top_k,
        seed=seed,
        resamples=resamples,
        confidence=confidence,
        graders=grader_names,
        rows=tuple(rows),
        slice_tables=tuple(
            SliceTable(
                name=name,
                questions=slice_question_counts[name],
                rows=tuple(slice_rows[name]),
            )
            for name in slice_names
        ),
        attribution_tables=tuple(attribution_tables),
        currency=prices.currency if prices is not None else None,
    )


def _observations(
    results: Sequence[QuestionResult],
    grader_names: Sequence[str],
    run_graders: frozenset[str],
) -> tuple[dict[str, list[float]], list[float], dict[str, GradeOutcomes]]:
    """Collect one table row's metric values, latencies and Grader outcomes."""
    metric_values: dict[str, list[float]] = {name: [] for name in METRIC_NAMES}
    latencies: list[float] = []
    for result in results:
        if result.status is not QuestionStatus.SCORED:
            continue
        latencies.append(result.latency_ms)
        if result.metrics is not None:
            values = result.metrics.to_json()
            for name in METRIC_NAMES:
                metric_values[name].append(values[name])
    grader_outcomes = {
        name: grade_outcomes(results, name) for name in grader_names if name in run_graders
    }
    return metric_values, latencies, grader_outcomes


def _token_values(run: AblationRun) -> list[float]:
    """Return the total tokens of every scored answer."""
    return [
        float(result.usage.total_tokens)
        for result in run.questions
        if result.status is QuestionStatus.SCORED and result.usage is not None
    ]


def _cost_observations(run: AblationRun, prices: PriceTable | None) -> CostObservations:
    """Return the cost pairs and the reasons a row's cost can be unavailable.

    A question enters the pairs only when its correctness Grade is ``ok`` and
    its usage is priced. Its cost is the generate call's usage priced by the
    generate step's model; a model the table does not price, or a trace with no
    generate step, leaves the row's cost unavailable rather than guessing.
    """
    if run.kind is not RunKind.ANSWER or prices is None:
        return CostObservations()
    pairs: list[tuple[float, float]] = []
    unpriced: set[str] = set()
    questions_without_model = 0
    for result in run.questions:
        if result.status is not QuestionStatus.SCORED:
            continue
        grade = result.grades.get(CORRECTNESS)
        if grade is None or grade.status is not GradeStatus.OK or grade.score is None:
            continue
        if result.usage is None:
            continue
        cost = question_cost(result.usage, result.trace, prices)
        if cost is None:
            model = generate_model(result.trace)
            if model is None:
                questions_without_model += 1
            else:
                unpriced.add(model)
            continue
        pairs.append((cost, grade.score))
    return CostObservations(
        pairs=tuple(pairs),
        unpriced_models=tuple(sorted(unpriced)),
        questions_without_model=questions_without_model,
    )


_Series = tuple[str, Sequence[Any], Callable[[Sequence[Any]], float]]


def _mean_series(
    metric_values: dict[str, list[float]],
    grader_outcomes: dict[str, GradeOutcomes],
) -> list[_Series]:
    """Return one table row's metric and Grader series, before any latency series."""
    return [
        *((name, metric_values[name], _resample_mean) for name in METRIC_NAMES),
        *((name, outcome.scores, _resample_mean) for name, outcome in grader_outcomes.items()),
    ]


def _row_estimates(
    series: Sequence[_Series],
    grader_names: Sequence[str],
    *,
    key_parts: Sequence[str],
    resamples: int,
    confidence: float,
) -> dict[str, MetricEstimate | None]:
    """Estimate every series of one table row, with an interval each.

    Each series draws from its own RNG keyed by ``key_parts`` and the series
    name, so a Slice row can name its Slice and reordering rows cannot move
    another row's interval.
    """
    estimates: dict[str, MetricEstimate | None] = {
        name: _estimate(
            observations,
            statistic,
            key=":".join((*key_parts, name)),
            resamples=resamples,
            confidence=confidence,
        )
        for name, observations, statistic in series
    }
    for name in grader_names:
        estimates.setdefault(name, None)
    return estimates


def _slice_names(run_files: Sequence[AblationRunFile]) -> tuple[str, ...]:
    """Return the Slices every question carries: question types, then ``hops-N`` by N."""
    names = {
        name
        for run_file in run_files
        for result in run_file.run.questions
        for name in result.slices
    }
    question_types = []
    hops = []
    for name in names:
        count = hop_count(name)
        if count is None:
            question_types.append(name)
        else:
            hops.append((count, name))
    return tuple(sorted(question_types) + [name for _, name in sorted(hops)])


def _slice_question_counts(
    run_files: Sequence[AblationRunFile], slice_names: Sequence[str]
) -> dict[str, int]:
    """Count each Slice's distinct Golden Set questions across the runs."""
    question_ids: dict[str, set[str]] = {name: set() for name in slice_names}
    for run_file in run_files:
        for result in run_file.run.questions:
            for name in result.slices:
                if name in question_ids:
                    question_ids[name].add(result.id)
    return {name: len(ids) for name, ids in question_ids.items()}


def _attribution_table(
    config: str, results: Sequence[QuestionResult], slice_names: Sequence[str]
) -> AttributionTable:
    """Count one answer run's labels, overall and per Slice."""
    scored = [result for result in results if result.status is QuestionStatus.SCORED]
    rows = [_attribution_row("All", scored)]
    rows += [
        _attribution_row(name, [result for result in scored if name in result.slices])
        for name in slice_names
    ]
    return AttributionTable(config=config, rows=tuple(rows))


def _attribution_row(name: str, results: Sequence[QuestionResult]) -> AttributionRow:
    """Count one attribution row's labels; every label is present even at zero."""
    counts = {label: 0 for label in FailureLabel}
    for result in results:
        counts[attribute_failure(result)] += 1
    return AttributionRow(name=name, graded=len(results), counts=counts)


def _report_grader_names(run_files: Sequence[AblationRunFile]) -> tuple[str, ...]:
    """Return the Grader columns: registry order first, then unknown names sorted."""
    present: set[str] = set()
    for run_file in run_files:
        if run_file.run.kind is RunKind.ANSWER:
            present.update(_graders_in_run(run_file.run))
    known = [name for name in default_grader_names() if name in present]
    return tuple(known + sorted(present - set(known)))


def _graders_in_run(run: AblationRun) -> frozenset[str]:
    """Return the Graders an Ablation Run names or holds a Grade for."""
    names = set(run.graders)
    for result in run.questions:
        names.update(result.grades)
    return frozenset(names)


def render_markdown(matrix: AblationMatrix) -> str:
    """Render the Ablation Matrix as a standalone Markdown document."""
    columns = _columns(matrix)
    lines = ["# Ablation Matrix", "", _preamble(matrix), ""]
    for label, value, is_code in _metadata(matrix):
        rendered = f"`{value}`" if is_code else value
        lines.append(f"- {label}: {rendered}")
    lines += ["", _CI_NOTE, ""]
    if _mixed_concurrency(matrix.rows):
        lines += [_concurrency_note(matrix.rows), ""]
    lines += _markdown_table(columns, matrix.rows)
    lines += _markdown_notes(matrix)
    lines += [
        "",
        "## Runs",
        "",
        "| " + " | ".join(_RUN_COLUMNS) + " |",
        "| " + " | ".join("---" for _ in _RUN_COLUMNS) + " |",
    ]
    lines += [
        f"| {_markdown_text(row.config)} | {_markdown_text(row.run_id)} | "
        f"{_markdown_text(row.harness_version)} |"
        for row in matrix.rows
    ]
    lines += ["", "## Slices", ""]
    lines += _markdown_slice_tables(matrix)
    lines += ["", "## Failure Attribution", ""]
    lines += _markdown_attribution_tables(matrix)
    return "\n".join(lines) + "\n"


def render_html(matrix: AblationMatrix) -> str:
    """Render the Ablation Matrix as one self-contained HTML document."""
    columns = _columns(matrix)
    lines = [
        "<!DOCTYPE html>",
        '<html lang="en">',
        "<head>",
        '<meta charset="utf-8">',
        '<meta name="viewport" content="width=device-width, initial-scale=1">',
        "<title>Ablation Matrix</title>",
        "<style>",
        _HTML_STYLE,
        "</style>",
        "</head>",
        "<body>",
        "<main>",
        "<h1>Ablation Matrix</h1>",
        f"<p>{escape(_preamble(matrix))}</p>",
        "<dl>",
    ]
    for label, value, is_code in _metadata(matrix):
        rendered = f"<code>{escape(value)}</code>" if is_code else escape(value)
        lines.append(f"<dt>{escape(label)}</dt><dd>{rendered}</dd>")
    lines += [
        "</dl>",
        f"<p>{escape(_CI_NOTE)}</p>",
    ]
    if _mixed_concurrency(matrix.rows):
        lines.append(f'<p class="note">{escape(_concurrency_note(matrix.rows))}</p>')
    lines += _html_table(
        columns,
        matrix.rows,
        caption=(
            f"Point estimate [{_confidence_label(matrix.confidence)} confidence interval]. "
            "Latency is in milliseconds."
        ),
    )
    lines += [f'<p class="note">{escape(note)}</p>' for note in _notes(matrix)]
    lines += [
        "<h2>Runs</h2>",
        '<div class="table-wrap">',
        "<table>",
        "<thead><tr>"
        + "".join(f"<th>{escape(label)}</th>" for label in _RUN_COLUMNS)
        + "</tr></thead>",
        "<tbody>",
        *(_html_run_row(row) for row in matrix.rows),
        "</tbody>",
        "</table>",
        "</div>",
        "<h2>Slices</h2>",
        *_html_slice_tables(matrix),
        "<h2>Failure Attribution</h2>",
        *_html_attribution_tables(matrix),
        "</main>",
        "</body>",
        "</html>",
    ]
    return "\n".join(lines) + "\n"


def _estimate[T](
    values: Sequence[T],
    statistic: Callable[[Sequence[T]], float],
    *,
    key: str,
    resamples: int,
    confidence: float,
) -> MetricEstimate | None:
    """Estimate one statistic with its interval, or ``None`` without values."""
    interval = bootstrap_ci(
        values,
        statistic,
        rng=random.Random(key),
        resamples=resamples,
        confidence=confidence,
    )
    if interval is None:
        return None
    return MetricEstimate(point=statistic(values), low=interval[0], high=interval[1])


def _resample_mean(values: Sequence[float]) -> float:
    """Return a resample's mean; a resample of a non-empty set is never empty."""
    return cast(float, mean(values))


def _resample_cost_per_correct(pairs: Sequence[tuple[float, float]]) -> float:
    """Return a resample's mean cost divided by its mean correctness score.

    A resample with no correct answer has a zero share, so its ratio is
    infinite; a question costs money whether or not it is right.
    """
    share = cast(float, mean([score for _, score in pairs]))
    if share == 0:
        return math.inf
    return cast(float, mean([cost for cost, _ in pairs])) / share


def _resample_percentile(percentile: int) -> Callable[[Sequence[float]], float]:
    """Return a bootstrap statistic for one nearest-rank latency percentile."""

    def statistic(values: Sequence[float]) -> float:
        return cast(float, nearest_rank_percentile(values, percentile))

    return statistic


def _bound_ranks(resamples: int, confidence: float) -> tuple[int, int]:
    """Return the 1-based sorted-sample ranks of the two confidence bounds.

    The tail is rounded once to basis points, so ``confidence=0.95`` gives
    exactly 2.5%, and the ranks are then computed with integer arithmetic.
    """
    tail = Fraction(round((1.0 - confidence) * 10_000), 20_000)
    lower = max(1, math.ceil(tail * resamples))
    upper = max(1, math.ceil((1 - tail) * resamples))
    return lower, upper


def _check_resamples(resamples: int) -> None:
    """Reject a bootstrap that would draw no resample."""
    if not isinstance(resamples, int) or isinstance(resamples, bool) or resamples < 1:
        raise ValueError(f"resamples must be a positive integer, got {resamples!r}")


def _check_confidence(confidence: float) -> None:
    """Reject a confidence level that has no two-sided interval."""
    if not 0.0 < confidence < 1.0:
        raise ValueError(f"confidence must be between 0 and 1, got {confidence!r}")


def _check_comparable(run_files: Sequence[AblationRunFile]) -> None:
    """Reject files whose Golden Set, top_k or config name cannot be compared."""
    first = run_files[0]
    seen: set[str] = set()
    for run_file in run_files:
        config = run_file.run.config.name
        if config in seen:
            raise ValueError(
                f"results file for retrieval config {config!r} was given more than once"
            )
        seen.add(config)
        if run_file.golden_set.sha256 != first.golden_set.sha256:
            raise ValueError(
                "results files were scored against different Golden Sets "
                f"(sha256 {first.golden_set.sha256} for {first.run.config.name!r} and "
                f"{run_file.golden_set.sha256} for {config!r}); their metrics do not compare"
            )
        if run_file.run.top_k != first.run.top_k:
            raise ValueError(
                "results files use different top_k values "
                f"({first.run.top_k} for {first.run.config.name!r} and "
                f"{run_file.run.top_k} for {config!r}); their metrics do not compare"
            )


def _preamble(matrix: AblationMatrix) -> str:
    """Return the opening sentence both renderers share."""
    files = _files_phrase(len(matrix.rows))
    if matrix.currency is None:
        return f"Built from {files} alone: the same files and seed give this report byte for byte."
    return (
        f"Built from {files} and the price table alone: the same files, seed and "
        "price table give this report byte for byte."
    )


def _metadata(matrix: AblationMatrix) -> list[tuple[str, str, bool]]:
    """Return the header's label, value and whether the value is a code span."""
    return [
        ("Golden Set", matrix.golden_set.path, True),
        ("Golden Set sha256", matrix.golden_set.sha256, True),
        ("Golden Set questions", str(matrix.golden_set.questions), False),
        ("top_k", str(matrix.top_k), False),
        (
            "Bootstrap",
            f"{_confidence_label(matrix.confidence)} confidence, "
            f"{matrix.resamples} resamples, seed {matrix.seed}",
            False,
        ),
    ]


def _files_phrase(count: int) -> str:
    """Return ``1 Ablation Run results file`` or ``3 Ablation Run results files``."""
    return f"{count} Ablation Run results {'file' if count == 1 else 'files'}"


def _confidence_label(confidence: float) -> str:
    """Format a confidence level as a percentage."""
    return f"{confidence:.0%}"


def _format_metric(estimate: MetricEstimate | None) -> str:
    """Format a metric cell as ``point [low, high]`` with three decimals."""
    if estimate is None:
        return "n/a"
    return f"{estimate.point:.3f} [{estimate.low:.3f}, {estimate.high:.3f}]"


def _format_one_decimal(estimate: MetricEstimate | None) -> str:
    """Format a latency or tokens cell with one decimal."""
    if estimate is None:
        return "n/a"
    return f"{estimate.point:.1f} [{estimate.low:.1f}, {estimate.high:.1f}]"


def _format_cost(estimate: MetricEstimate | None) -> str:
    """Format a cost cell with four significant digits; an infinite point is just ``∞``."""
    if estimate is None:
        return "—"
    if math.isinf(estimate.point):
        return "∞"
    return (
        f"{_format_cost_number(estimate.point)} "
        f"[{_format_cost_number(estimate.low)}, {_format_cost_number(estimate.high)}]"
    )


def _format_cost_number(value: float) -> str:
    """Format one cost bound with four significant digits, trailing zeros kept."""
    if math.isinf(value):
        return "∞"
    return f"{value:#.4g}".removesuffix(".")


def _cell(
    name: str, formatter: Callable[[MetricEstimate | None], str]
) -> Callable[[MatrixRow], str]:
    """Return the formatter for one estimate cell of a matrix row."""
    return lambda row: formatter(row.estimates.get(name))


def _latency_cell(name: str) -> Callable[[MatrixRow], str]:
    """Return a latency cell, gated when the run did not measure provider latency."""

    def format_cell(row: MatrixRow) -> str:
        if row.model_mode in _MEASURED_MODEL_MODES:
            return _format_one_decimal(row.estimates.get(name))
        if row.model_mode is ModelMode.UNKNOWN:
            return "unknown"
        return "replayed"

    return format_cell


def _tokens_cell(row: MatrixRow) -> str:
    """Return a Tokens cell; a retrieval run made no chat call to count."""
    if row.kind is not RunKind.ANSWER:
        return "—"
    return _format_one_decimal(row.estimates.get(TOKENS))


def _cost_cell(row: MatrixRow) -> str:
    """Return a Cost per Correct Answer cell; a missing estimate is a dash."""
    return _format_cost(row.estimates.get(COST_PER_CORRECT))


def _cost_label(currency: str | None) -> str:
    """Return the cost column's header, naming the currency when a table gives one."""
    if currency is None:
        return "Cost per Correct Answer"
    return f"Cost per Correct Answer ({currency})"


def _grader_cell(name: str) -> Callable[[MatrixRow | SliceRow], str]:
    """Return the formatter for one Grader column, with its non-zero counts beside it."""

    def format_cell(row: MatrixRow | SliceRow) -> str:
        if name not in row.graders:
            return "—"
        estimate = row.estimates.get(name)
        parts = ["—" if estimate is None else _format_metric(estimate)]
        outcomes = row.grader_outcomes.get(name)
        if outcomes is not None and outcomes.grader_error:
            count = outcomes.grader_error
            parts.append(f"{count} {'error' if count == 1 else 'errors'}")
        if outcomes is not None and outcomes.not_applicable:
            parts.append(f"{outcomes.not_applicable} n/a")
        return " · ".join(parts)

    return format_cell


_CI_NOTE = (
    "Confidence intervals are percentile bootstrap intervals over the observations each "
    "estimate uses; a Grader's interval covers its ok grades and excludes grader_error and "
    "not_applicable."
)

_LATENCY_NOTE = (
    "Latency is reported only for live and passthrough runs: a record run mixes "
    "recorded upstream calls with replayed ones, so its latency measures neither the "
    "provider nor the replay; a replay run replays every call, and a run from before "
    "model modes is unknown, so those cells show replayed or unknown."
)

_NO_PRICE_TABLE_NOTE = (
    "No price table was given, so Cost per Correct Answer is —; pass --prices with a "
    "price for each model the generate step names."
)

_COST_SCOPE_NOTE = (
    "Cost covers only the generate call: Trelis's debug object reports only the chat "
    "call's usage, so embedding, rerank and the judge are not included."
)


def _notes(matrix: AblationMatrix) -> list[str]:
    """Return the notes both renderers put under the main matrix."""
    notes = []
    if _latency_gated(matrix.rows):
        notes.append(_LATENCY_NOTE)
    if matrix.currency is None:
        if any(_needs_a_price_table(row) for row in matrix.rows):
            notes.append(_NO_PRICE_TABLE_NOTE)
    elif any(row.kind is RunKind.ANSWER for row in matrix.rows):
        notes.append(_COST_SCOPE_NOTE)
    notes += [
        _unpriced_note(row)
        for row in matrix.rows
        if row.cost.unpriced_models or row.cost.questions_without_model
    ]
    return notes


def _needs_a_price_table(row: MatrixRow) -> bool:
    """Say whether a row has an ok correctness Grade that a price table would price."""
    if row.kind is not RunKind.ANSWER:
        return False
    outcomes = row.grader_outcomes.get(CORRECTNESS)
    return outcomes is not None and bool(outcomes.scores)


def _latency_gated(rows: Sequence[MatrixRow]) -> bool:
    """Say whether any row's latency did not measure the provider."""
    return any(row.model_mode not in _MEASURED_MODEL_MODES for row in rows)


def _unpriced_note(row: MatrixRow) -> str:
    """Explain one row whose Cost per Correct Answer cannot be priced."""
    reasons = []
    models = row.cost.unpriced_models
    if len(models) == 1:
        reasons.append(f"the price table has no entry for model {models[0]}")
    elif models:
        reasons.append("the price table has no entry for models " + ", ".join(models))
    missing = row.cost.questions_without_model
    if missing:
        noun = "answer has" if missing == 1 else "answers have"
        reasons.append(f"{missing} {noun} no generate step")
    return f"Cost per Correct Answer is — for {row.config}: " + "; ".join(reasons) + "."


def _columns(matrix: AblationMatrix) -> tuple[tuple[str, Callable[[MatrixRow], str]], ...]:
    """Return the matrix columns, with one Grader column per Grader in the report."""
    return (
        ("Retrieval Config", lambda row: row.config),
        *((METRIC_LABELS[name], _cell(name, _format_metric)) for name in METRIC_NAMES),
        *((grader_label(name), _grader_cell(name)) for name in matrix.graders),
        ("Latency p50 (ms)", _latency_cell(LATENCY_P50)),
        ("Latency p95 (ms)", _latency_cell(LATENCY_P95)),
        ("Tokens", _tokens_cell),
        (_cost_label(matrix.currency), _cost_cell),
        ("Scored", lambda row: str(row.scored)),
        ("Skipped", lambda row: str(row.skipped)),
        ("Error", lambda row: str(row.error)),
        ("Concurrency", lambda row: str(row.concurrency)),
    )


_RUN_COLUMNS = ("Retrieval Config", "Run ID", "Harness version")

ATTRIBUTION_LABELS = {
    FailureLabel.CORRECT: "Correct",
    FailureLabel.RETRIEVAL_MISS: "Retrieval miss",
    FailureLabel.GENERATION_ERROR: "Generation error",
    FailureLabel.CITATION_ERROR: "Citation error",
    FailureLabel.UNATTRIBUTED: "Unattributed",
}

_NO_SLICES = "No Slice in this report, so there is nothing to break down."
_NO_ANSWER_RUN = "No answer Ablation Run in this report, so there is nothing to attribute."


def _slice_columns(matrix: AblationMatrix) -> tuple[tuple[str, Callable[[SliceRow], str]], ...]:
    """Return a Slice table's columns: metrics, Graders and the scored count."""
    return (
        ("Retrieval Config", lambda row: row.config),
        *((METRIC_LABELS[name], _slice_cell(name, _format_metric)) for name in METRIC_NAMES),
        *((grader_label(name), _slice_grader_cell(name)) for name in matrix.graders),
        ("Scored", _dash_without_questions(lambda row: str(row.scored))),
    )


def _dash_without_questions(
    format_cell: Callable[[SliceRow], str],
) -> Callable[[SliceRow], str]:
    """Return a Slice cell that is dashed when the config has no question in the Slice."""

    def format_with_dash(row: SliceRow) -> str:
        if row.questions == 0:
            return "—"
        return format_cell(row)

    return format_with_dash


def _slice_cell(
    name: str, formatter: Callable[[MetricEstimate | None], str]
) -> Callable[[SliceRow], str]:
    """Return a Slice metric cell, dashed when the config has no question there."""
    return _dash_without_questions(lambda row: formatter(row.estimates.get(name)))


def _slice_grader_cell(name: str) -> Callable[[SliceRow], str]:
    """Return a Slice Grader cell, dashed when the config has no question there."""
    return _dash_without_questions(_grader_cell(name))


def _attribution_columns() -> tuple[tuple[str, Callable[[AttributionRow], str]], ...]:
    """Return the Failure Attribution columns, each count with its share of Graded."""
    return (
        ("Slice", lambda row: row.name),
        ("Graded", lambda row: str(row.graded)),
        *((ATTRIBUTION_LABELS[label], _share_cell(label)) for label in ATTRIBUTION_LABELS),
    )


def _share_cell(label: FailureLabel) -> Callable[[AttributionRow], str]:
    """Return one Failure Attribution cell: the label's count and its share."""

    def format_cell(row: AttributionRow) -> str:
        return _format_share(row.counts[label], row.graded)

    return format_cell


def _format_share(count: int, graded: int) -> str:
    """Format a count with its share of the row's graded questions."""
    if graded == 0:
        return f"{count} (—)"
    return f"{count} ({count / graded:.0%})"


def _questions_phrase(count: int) -> str:
    """Return ``1 question`` or ``3 questions``."""
    return f"{count} {'question' if count == 1 else 'questions'}"


def _markdown_slice_tables(matrix: AblationMatrix) -> list[str]:
    """Render one Markdown table per Slice, separated by a blank line."""
    if not matrix.slice_tables:
        return [_NO_SLICES]
    columns = _slice_columns(matrix)
    lines: list[str] = []
    for index, table in enumerate(matrix.slice_tables):
        if index:
            lines.append("")
        lines += [f"### {_markdown_text(table.name)}", "", _questions_phrase(table.questions), ""]
        lines += _markdown_table(columns, table.rows)
    return lines


def _markdown_attribution_tables(matrix: AblationMatrix) -> list[str]:
    """Render one Markdown Failure Attribution table per answer run."""
    if not matrix.attribution_tables:
        return [_NO_ANSWER_RUN]
    columns = _attribution_columns()
    lines: list[str] = []
    for index, table in enumerate(matrix.attribution_tables):
        if index:
            lines.append("")
        lines += [f"### {_markdown_text(table.config)}", ""]
        lines += _markdown_table(columns, table.rows)
    return lines


def _html_slice_tables(matrix: AblationMatrix) -> list[str]:
    """Render one HTML table per Slice, headed by the Slice and its question count."""
    if not matrix.slice_tables:
        return [f"<p>{escape(_NO_SLICES)}</p>"]
    columns = _slice_columns(matrix)
    lines: list[str] = []
    for table in matrix.slice_tables:
        lines += [
            f"<h3>{escape(table.name)}</h3>",
            f'<p class="note">{_questions_phrase(table.questions)}</p>',
            *_html_table(columns, table.rows),
        ]
    return lines


def _html_attribution_tables(matrix: AblationMatrix) -> list[str]:
    """Render one HTML Failure Attribution table per answer run."""
    if not matrix.attribution_tables:
        return [f"<p>{escape(_NO_ANSWER_RUN)}</p>"]
    columns = _attribution_columns()
    lines: list[str] = []
    for table in matrix.attribution_tables:
        lines += [
            f"<h3>{escape(table.config)}</h3>",
            *_html_table(columns, table.rows),
        ]
    return lines


def _markdown_notes(matrix: AblationMatrix) -> list[str]:
    """Return the notes under the Markdown matrix, each as its own paragraph."""
    return [line for note in _notes(matrix) for line in ("", note)]


def _markdown_table[RowT](
    columns: Sequence[tuple[str, Callable[[RowT], str]]],
    rows: Sequence[RowT],
) -> list[str]:
    """Format one Markdown table: its header, separator and one line per row."""
    lines = [
        "| " + " | ".join(label for label, _ in columns) + " |",
        "| " + " | ".join("---" for _ in columns) + " |",
    ]
    return lines + [_markdown_row(row, columns) for row in rows]


def _markdown_row[RowT](row: RowT, columns: Sequence[tuple[str, Callable[[RowT], str]]]) -> str:
    """Format one Markdown table row."""
    cells = (_markdown_text(format_cell(row)) for _, format_cell in columns)
    return "| " + " | ".join(cells) + " |"


def _markdown_text(value: str) -> str:
    """Keep a value from ending a Markdown table cell early."""
    return value.replace("|", "\\|")


def _html_table[RowT](
    columns: Sequence[tuple[str, Callable[[RowT], str]]],
    rows: Sequence[RowT],
    *,
    caption: str | None = None,
) -> list[str]:
    """Format one HTML table, with an optional caption, as a scrollable block."""
    lines = ['<div class="table-wrap">', "<table>"]
    if caption is not None:
        lines.append(f"<caption>{escape(caption)}</caption>")
    lines += [
        "<thead><tr>",
        *(
            f"<th>{escape(label)}</th>" if index == 0 else f'<th class="num">{escape(label)}</th>'
            for index, (label, _) in enumerate(columns)
        ),
        "</tr></thead>",
        "<tbody>",
        *(_html_row(row, columns) for row in rows),
        "</tbody>",
        "</table>",
        "</div>",
    ]
    return lines


def _html_row[RowT](row: RowT, columns: Sequence[tuple[str, Callable[[RowT], str]]]) -> str:
    """Format one HTML table row."""
    cells = [
        f"<td>{escape(format_cell(row))}</td>"
        if index == 0
        else f'<td class="num">{escape(format_cell(row))}</td>'
        for index, (_, format_cell) in enumerate(columns)
    ]
    return "<tr>" + "".join(cells) + "</tr>"


def _html_run_row(row: MatrixRow) -> str:
    """Format one Runs table row."""
    return (
        "<tr>"
        f"<td>{escape(row.config)}</td>"
        f"<td><code>{escape(row.run_id)}</code></td>"
        f"<td>{escape(row.harness_version)}</td>"
        "</tr>"
    )


def _mixed_concurrency(rows: Sequence[MatrixRow]) -> bool:
    """Say whether the rows' latencies were measured under different load."""
    return len({row.concurrency for row in rows}) > 1


def _concurrency_note(rows: Sequence[MatrixRow]) -> str:
    """Explain that latency compares only at one concurrency."""
    levels = ", ".join(str(value) for value in sorted({row.concurrency for row in rows}))
    return (
        f"These runs used different concurrency ({levels}); compare latency only "
        "within one concurrency."
    )


_HTML_STYLE = """\
:root {
  color-scheme: light dark;
  --page: #ffffff;
  --ink: #1d1d1f;
  --muted: #5f6368;
  --line: #d8d8dd;
  --head: #f4f4f6;
}
@media (prefers-color-scheme: dark) {
  :root {
    --page: #17171b;
    --ink: #ececf1;
    --muted: #a0a0aa;
    --line: #3a3a42;
    --head: #232329;
  }
}
body {
  margin: 0;
  padding: 2rem 1rem;
  background: var(--page);
  color: var(--ink);
  font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
}
main { max-width: 72rem; margin: 0 auto; }
h1 { margin: 0 0 0.5rem; font-size: 1.75rem; }
h2 { margin-top: 2rem; font-size: 1.25rem; }
h3 { margin-top: 1.5rem; font-size: 1.05rem; }
p { color: var(--muted); }
code { font-size: 0.9em; }
dl {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 0.15rem 1rem;
  margin: 1.5rem 0;
}
dt { color: var(--muted); }
dd { margin: 0; }
.table-wrap { overflow-x: auto; }
table {
  border-collapse: collapse;
  width: 100%;
  margin: 1rem 0 1.5rem;
  font-size: 0.95rem;
}
caption { text-align: left; color: var(--muted); padding-bottom: 0.4rem; }
th, td {
  border-bottom: 1px solid var(--line);
  padding: 0.45rem 0.6rem;
  text-align: left;
  vertical-align: top;
}
thead th { background: var(--head); font-weight: 600; white-space: nowrap; }
td.num, th.num { text-align: right; white-space: nowrap; }
.note { font-size: 0.9rem; }
"""
