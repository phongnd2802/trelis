"""The Ablation Matrix report: bootstrap confidence intervals over results files.

The report is built from Ablation Run results files alone. Every Retrieval
Config gets a row with the four metric means and the two latency percentiles,
each with a seeded percentile-bootstrap confidence interval, plus the counts
and the run's concurrency. The same files and seed render byte-identical
Markdown and HTML: no wall-clock time, no machine paths and no randomness
outside the seeded RNG. ``eval/docs/metrics.md`` holds the definitions.
"""

from __future__ import annotations

import math
import random
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from fractions import Fraction
from html import escape
from typing import cast

from .ablation_run import (
    METRIC_NAMES,
    MRR,
    NDCG_10,
    RECALL_5,
    RECALL_10,
    AblationRunFile,
    GoldenSetRef,
    QuestionStatus,
)
from .metrics import mean, nearest_rank_percentile

LATENCY_P50 = "latency_p50"
LATENCY_P95 = "latency_p95"

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
class MatrixRow:
    """One Retrieval Config's Ablation Run, with every estimate's interval."""

    config: str
    run_id: str
    harness_version: str
    concurrency: int
    scored: int
    skipped: int
    error: int
    estimates: dict[str, MetricEstimate | None]


@dataclass(frozen=True)
class AblationMatrix:
    """One row per Retrieval Config, with the metadata the rows share."""

    golden_set: GoldenSetRef
    top_k: int
    seed: int
    resamples: int
    confidence: float
    rows: tuple[MatrixRow, ...]


def bootstrap_ci(
    values: Sequence[float],
    statistic: Callable[[Sequence[float]], float],
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
    constant values give the value at both bounds.
    """
    _check_resamples(resamples)
    _check_confidence(confidence)
    if not values:
        return None
    if len(set(values)) == 1:
        return values[0], values[0]

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
) -> AblationMatrix:
    """Build the Ablation Matrix from loaded Ablation Run results files.

    Rows keep the order of ``run_files``. A metric's point estimate is the mean
    over its scored questions, so it equals the results file's summary, and a
    latency percentile uses the same nearest-rank rule. Each interval draws
    from its own RNG keyed by the seed, config name and metric, so reordering
    rows cannot change another row's interval.
    """
    _check_resamples(resamples)
    _check_confidence(confidence)
    if not run_files:
        raise ValueError("no results files to report on")
    _check_comparable(run_files)

    rows = []
    for run_file in run_files:
        run = run_file.run
        metric_values: dict[str, list[float]] = {name: [] for name in METRIC_NAMES}
        latencies: list[float] = []
        for result in run.questions:
            if result.status is not QuestionStatus.SCORED or result.metrics is None:
                continue
            values = result.metrics.to_json()
            for name in METRIC_NAMES:
                metric_values[name].append(values[name])
            latencies.append(result.latency_ms)

        config = run.config.name
        series = [
            *((name, metric_values[name], _resample_mean) for name in METRIC_NAMES),
            (LATENCY_P50, latencies, _resample_percentile(50)),
            (LATENCY_P95, latencies, _resample_percentile(95)),
        ]
        rows.append(
            MatrixRow(
                config=config,
                run_id=run_file.run_id,
                harness_version=run_file.harness_version,
                concurrency=run.concurrency,
                scored=run.summary.scored,
                skipped=run.summary.skipped,
                error=run.summary.error,
                estimates={
                    name: _estimate(
                        observations,
                        statistic,
                        seed=seed,
                        config=config,
                        metric=name,
                        resamples=resamples,
                        confidence=confidence,
                    )
                    for name, observations, statistic in series
                },
            )
        )

    first = run_files[0]
    return AblationMatrix(
        golden_set=first.golden_set,
        top_k=first.run.top_k,
        seed=seed,
        resamples=resamples,
        confidence=confidence,
        rows=tuple(rows),
    )


def render_markdown(matrix: AblationMatrix) -> str:
    """Render the Ablation Matrix as a standalone Markdown document."""
    lines = ["# Ablation Matrix", "", _preamble(matrix), ""]
    for label, value, is_code in _metadata(matrix):
        rendered = f"`{value}`" if is_code else value
        lines.append(f"- {label}: {rendered}")
    lines += ["", _CI_NOTE, ""]
    if _mixed_concurrency(matrix.rows):
        lines += [_concurrency_note(matrix.rows), ""]
    lines += [
        "| " + " | ".join(label for label, _ in _COLUMNS) + " |",
        "| " + " | ".join("---" for _ in _COLUMNS) + " |",
    ]
    lines += [_markdown_matrix_row(row) for row in matrix.rows]
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
    return "\n".join(lines) + "\n"


def render_html(matrix: AblationMatrix) -> str:
    """Render the Ablation Matrix as one self-contained HTML document."""
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
    lines += [
        '<div class="table-wrap">',
        "<table>",
        f"<caption>Point estimate [{_confidence_label(matrix.confidence)} confidence interval]. "
        "Latency is in milliseconds.</caption>",
        "<thead><tr>",
        *(
            f"<th>{escape(label)}</th>" if index == 0 else f'<th class="num">{escape(label)}</th>'
            for index, (label, _) in enumerate(_COLUMNS)
        ),
        "</tr></thead>",
        "<tbody>",
        *(_html_matrix_row(row) for row in matrix.rows),
        "</tbody>",
        "</table>",
        "</div>",
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
        "</main>",
        "</body>",
        "</html>",
    ]
    return "\n".join(lines) + "\n"


def _estimate(
    values: Sequence[float],
    statistic: Callable[[Sequence[float]], float],
    *,
    seed: int,
    config: str,
    metric: str,
    resamples: int,
    confidence: float,
) -> MetricEstimate | None:
    """Estimate one statistic with its interval, or ``None`` without values."""
    interval = bootstrap_ci(
        values,
        statistic,
        rng=random.Random(f"{seed}:{config}:{metric}"),
        resamples=resamples,
        confidence=confidence,
    )
    if interval is None:
        return None
    return MetricEstimate(point=statistic(values), low=interval[0], high=interval[1])


def _resample_mean(values: Sequence[float]) -> float:
    """Return a resample's mean; a resample of a non-empty set is never empty."""
    return cast(float, mean(values))


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
    return (
        f"Built from {_files_phrase(len(matrix.rows))} alone: the same files "
        "and seed give this report byte for byte."
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


def _format_latency(estimate: MetricEstimate | None) -> str:
    """Format a latency cell in milliseconds with one decimal."""
    if estimate is None:
        return "n/a"
    return f"{estimate.point:.1f} [{estimate.low:.1f}, {estimate.high:.1f}]"


def _cell(
    name: str, formatter: Callable[[MetricEstimate | None], str]
) -> Callable[[MatrixRow], str]:
    """Return the formatter for one estimate cell of a matrix row."""
    return lambda row: formatter(row.estimates[name])


_CI_NOTE = "Confidence intervals are percentile bootstrap intervals over the scored questions."

_COLUMNS: tuple[tuple[str, Callable[[MatrixRow], str]], ...] = (
    ("Retrieval Config", lambda row: row.config),
    *((METRIC_LABELS[name], _cell(name, _format_metric)) for name in METRIC_NAMES),
    ("Latency p50 (ms)", _cell(LATENCY_P50, _format_latency)),
    ("Latency p95 (ms)", _cell(LATENCY_P95, _format_latency)),
    ("Scored", lambda row: str(row.scored)),
    ("Skipped", lambda row: str(row.skipped)),
    ("Error", lambda row: str(row.error)),
    ("Concurrency", lambda row: str(row.concurrency)),
)

_RUN_COLUMNS = ("Retrieval Config", "Run ID", "Harness version")


def _markdown_matrix_row(row: MatrixRow) -> str:
    """Format one Ablation Matrix table row."""
    cells = (_markdown_text(format_cell(row)) for _, format_cell in _COLUMNS)
    return "| " + " | ".join(cells) + " |"


def _markdown_text(value: str) -> str:
    """Keep a value from ending a Markdown table cell early."""
    return value.replace("|", "\\|")


def _html_matrix_row(row: MatrixRow) -> str:
    """Format one Ablation Matrix table row."""
    cells = [
        f"<td>{escape(format_cell(row))}</td>"
        if index == 0
        else f'<td class="num">{escape(format_cell(row))}</td>'
        for index, (_, format_cell) in enumerate(_COLUMNS)
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
