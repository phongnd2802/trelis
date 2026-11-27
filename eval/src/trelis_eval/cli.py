"""The trelis-eval command line."""

from __future__ import annotations

import argparse
import os
import sys
import time
from collections import Counter
from collections.abc import Sequence
from pathlib import Path

import httpx

from .ablation_run import (
    HARNESS_VERSION,
    MAX_CONCURRENCY,
    AblationAborted,
    AblationRun,
    AblationRunFile,
    GoldenSetRef,
    ManifestRef,
    QuestionStatus,
    default_run_id,
    file_sha256,
    load_manifest,
    parse_retrieval_config,
    run_ablation,
    write_ablation_run,
)
from .client import TrelisClient, TrelisError
from .golden import GoldenSetError, load_golden_set, write_golden_set
from .ingest import IngestRecord, IngestStatus, ingest, write_manifest
from .multihop import (
    MultiHopDataError,
    fetch,
    load_corpus,
    load_questions,
    select_evidence_documents,
    select_subset,
    stratified_sample,
    to_golden_set,
)

DEFAULT_DATA_DIR = Path("eval/data/multihop-rag")
DEFAULT_BASE_URL = "http://localhost:8080"

DEFAULT_REQUEST_TIMEOUT = 120.0
"""Seconds one POST /retrieve may take. Trelis embeds the query with a hosted
model inside the request, and a slow provider answer must not turn into a
question error that biases the means."""


class CliError(RuntimeError):
    """A command cannot run, with a message meant for the person running it."""


def main(
    argv: Sequence[str] | None = None,
    *,
    transport: httpx.BaseTransport | None = None,
) -> int:
    """Run one command and return its exit code (0 on success)."""
    args = _build_parser().parse_args(argv)
    try:
        return args.handler(args, transport)
    except (
        GoldenSetError,
        MultiHopDataError,
        TrelisError,
        CliError,
        ValueError,
        OSError,
        httpx.HTTPError,
    ) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


def _build_parser() -> argparse.ArgumentParser:
    """Build the argparse tree for all trelis-eval commands."""
    parser = argparse.ArgumentParser(
        prog="trelis-eval",
        description="External eval harness for Trelis, over the public HTTP API.",
    )
    commands = parser.add_subparsers(dest="command", required=True)

    multihop = commands.add_parser("multihop", help="work with the MultiHop-RAG Benchmark Corpus")
    multihop_commands = multihop.add_subparsers(dest="multihop_command", required=True)

    fetch_command = multihop_commands.add_parser(
        "fetch", help="download corpus.json and MultiHopRAG.json"
    )
    fetch_command.add_argument("--data-dir", type=Path, default=DEFAULT_DATA_DIR)
    fetch_command.set_defaults(handler=_run_fetch)

    golden_command = multihop_commands.add_parser(
        "golden", help="write the full MultiHop-RAG Golden Set"
    )
    golden_command.add_argument("--data-dir", type=Path, default=DEFAULT_DATA_DIR)
    golden_command.add_argument("--out", type=Path, required=True)
    golden_command.set_defaults(handler=_run_multihop_golden)

    ingest_command = multihop_commands.add_parser(
        "ingest",
        help="ingest a subset into Trelis, wait for it, and write the Golden Set and manifest",
        epilog=(
            "A re-run finds earlier uploads through duplicate detection: a Document "
            "that failed before is reported failed again, and --principals/--groups "
            "do not change an existing Access List."
        ),
    )
    ingest_command.add_argument("--data-dir", type=Path, default=DEFAULT_DATA_DIR)
    ingest_command.add_argument(
        "--questions", type=_positive_int, required=True, help="number of Golden questions"
    )
    ingest_command.add_argument(
        "--extra-docs", type=_non_negative_int, default=0, help="distractor documents"
    )
    ingest_command.add_argument("--seed", type=int, default=0, help="selection seed")
    ingest_command.add_argument(
        "--sample",
        choices=("first", "stratified"),
        default="first",
        help="take the first questions or a question-type-stratified sample",
    )
    ingest_command.add_argument(
        "--principals", default="", help="comma-separated Principal names for the Access List"
    )
    ingest_command.add_argument(
        "--groups", default="", help="comma-separated Group names for the Access List"
    )
    ingest_command.add_argument("--golden-out", type=Path, help="subset Golden Set JSONL path")
    ingest_command.add_argument("--manifest-out", type=Path, help="manifest JSONL path")
    ingest_command.add_argument(
        "--poll-interval", type=float, default=1.0, help="seconds between Document polls"
    )
    ingest_command.add_argument(
        "--timeout", type=float, default=300.0, help="seconds to wait for every Document"
    )
    ingest_command.add_argument(
        "--base-url", default=None, help="Trelis base URL (default: $TRELIS_BASE_URL)"
    )
    ingest_command.set_defaults(handler=_run_ingest)

    golden_cli = commands.add_parser("golden", help="work with Golden Set files")
    golden_commands = golden_cli.add_subparsers(dest="golden_command", required=True)
    validate_command = golden_commands.add_parser(
        "validate", help="validate a Golden Set and print its Slice counts"
    )
    validate_command.add_argument("path", type=Path)
    validate_command.set_defaults(handler=_run_golden_validate)

    retrieval = commands.add_parser("retrieval", help="run retrieval ablations")
    retrieval_commands = retrieval.add_subparsers(dest="retrieval_command", required=True)
    run_command = retrieval_commands.add_parser(
        "run", help="score a Golden Set under one or more Retrieval Configs"
    )
    run_command.add_argument("--golden", type=Path, required=True)
    run_command.add_argument("--manifest", type=Path, required=True)
    run_command.add_argument(
        "--config",
        action="append",
        dest="configs",
        metavar="TECHNIQUES",
        help="Retrieval Techniques joined with '+' (repeatable; default: dense)",
    )
    run_command.add_argument(
        "--top-k",
        type=int,
        default=10,
        help="Chunks to retrieve per question, between 10 and 100",
    )
    run_command.add_argument(
        "--concurrency",
        type=_concurrency,
        default=1,
        help=f"questions in flight at once, 1 to {MAX_CONCURRENCY} (recorded in the results "
        "header, since latency is measured under it)",
    )
    run_command.add_argument(
        "--request-timeout",
        type=_positive_float,
        default=DEFAULT_REQUEST_TIMEOUT,
        help="seconds to wait for one POST /retrieve, including Trelis's own model call",
    )
    run_command.add_argument(
        "--out-dir", type=Path, default=Path("eval/runs"), help="directory for results files"
    )
    run_command.add_argument(
        "--run-id", default=None, help="results directory name (default: UTC timestamp)"
    )
    run_command.add_argument(
        "--base-url", default=None, help="Trelis base URL (default: $TRELIS_BASE_URL)"
    )
    run_command.set_defaults(handler=_run_retrieval)

    return parser


def _run_fetch(args: argparse.Namespace, transport: httpx.BaseTransport | None) -> int:
    """Download the MultiHop-RAG files that are not present yet."""
    del transport
    for path in fetch(args.data_dir):
        print(path)
    return 0


def _run_multihop_golden(args: argparse.Namespace, transport: httpx.BaseTransport | None) -> int:
    """Write the full MultiHop-RAG Golden Set as JSONL."""
    del transport
    questions = load_questions(args.data_dir / "MultiHopRAG.json")
    golden = to_golden_set(questions)
    write_golden_set(args.out, golden)
    print(f"Wrote {len(golden)} Golden questions to {args.out}")
    return 0


def _run_golden_validate(args: argparse.Namespace, transport: httpx.BaseTransport | None) -> int:
    """Load a Golden Set with validation and print its Slice counts."""
    del transport
    questions = load_golden_set(args.path)
    slices = Counter(name for question in questions for name in question.slices)
    print(f"Golden Set: {len(questions)} questions")
    for name in sorted(slices):
        print(f"  {name}: {slices[name]}")
    return 0


def _run_ingest(args: argparse.Namespace, transport: httpx.BaseTransport | None) -> int:
    """Ingest the selected subset, then write its Golden Set and manifest."""
    base_url, api_key = _connection(args.base_url)
    documents = load_corpus(args.data_dir / "corpus.json")
    questions = load_questions(args.data_dir / "MultiHopRAG.json")
    golden = to_golden_set(questions)
    if args.sample == "stratified":
        selected_questions = stratified_sample(golden, args.questions, args.seed)
        selected_documents = select_evidence_documents(
            documents, selected_questions, args.extra_docs, args.seed
        )
    else:
        selected_questions, selected_documents = select_subset(
            documents, golden, args.questions, args.extra_docs, args.seed
        )

    with TrelisClient(base_url, api_key, transport=transport) as client:
        records = ingest(
            client,
            selected_documents,
            _comma_separated(args.principals),
            _comma_separated(args.groups),
            args.poll_interval,
            args.timeout,
        )

    golden_out = args.golden_out or args.data_dir / "subset-golden.jsonl"
    manifest_out = args.manifest_out or args.data_dir / "manifest.jsonl"
    write_golden_set(golden_out, selected_questions)
    write_manifest(manifest_out, records)
    _print_summary(records, golden_out, manifest_out, len(selected_questions))
    if any(
        record.status in (IngestStatus.FAILED, IngestStatus.ERROR, IngestStatus.TIMEOUT)
        for record in records
    ):
        return 1
    return 0


def _run_retrieval(args: argparse.Namespace, transport: httpx.BaseTransport | None) -> int:
    """Run every Retrieval Config in turn and write one results file each."""
    configs = [parse_retrieval_config(value) for value in (args.configs or ["dense"])]
    names = [config.name for config in configs]
    duplicates = sorted({name for name in names if names.count(name) > 1})
    if duplicates:
        raise CliError(f"retrieval config {duplicates[0]!r} was given more than once")
    if not 10 <= args.top_k <= 100:
        raise CliError(f"--top-k must be between 10 and 100, got {args.top_k}")

    golden = load_golden_set(args.golden)
    manifest = load_manifest(args.manifest)
    run_id = args.run_id or default_run_id()
    paths = [args.out_dir / run_id / f"{config.name}.jsonl" for config in configs]
    existing = [str(path) for path in paths if path.exists()]
    if existing:
        raise CliError(
            "results file(s) already exist; choose another --run-id: " + ", ".join(existing)
        )

    base_url, api_key = _connection(args.base_url)
    golden_set = GoldenSetRef(
        path=str(args.golden),
        sha256=file_sha256(args.golden),
        questions=len(golden),
    )
    manifest_ref = ManifestRef(path=str(args.manifest), sha256=file_sha256(args.manifest))

    exit_code = 0
    with TrelisClient(
        base_url, api_key, transport=transport, timeout=args.request_timeout
    ) as client:
        for config, path in zip(configs, paths, strict=True):
            try:
                run = run_ablation(
                    client,
                    golden,
                    manifest,
                    config,
                    args.top_k,
                    clock=time.monotonic,
                    concurrency=args.concurrency,
                )
            except AblationAborted as exc:
                print(f"config {config.name}: aborted: {exc}")
                exit_code = 1
                continue
            write_ablation_run(
                path,
                AblationRunFile(
                    run_id=run_id,
                    golden_set=golden_set,
                    manifest=manifest_ref,
                    base_url=base_url,
                    harness_version=HARNESS_VERSION,
                    run=run,
                ),
            )
            _print_retrieval_summary(run, path)
            if run.summary.scored == 0:
                exit_code = 1
    return exit_code


def _print_retrieval_summary(run: AblationRun, path: Path) -> None:
    """Print one Ablation Run's counts, metric means, latencies and file."""
    summary = run.summary

    def metric(value: float | None) -> str:
        return "n/a" if value is None else f"{value:.4f}"

    def latency(value: float | None) -> str:
        return "n/a" if value is None else f"{value:.1f}ms"

    line = (
        f"{run.config.name}: scored={summary.scored} skipped={summary.skipped} "
        f"error={summary.error} recall@5={metric(summary.recall_at_5)} "
        f"recall@10={metric(summary.recall_at_10)} mrr={metric(summary.mrr)} "
        f"ndcg@10={metric(summary.ndcg_at_10)} p50={latency(summary.latency_p50)} "
        f"p95={latency(summary.latency_p95)}"
    )
    first_error = next(
        (
            result.error
            for result in run.questions
            if result.status is QuestionStatus.ERROR and result.error is not None
        ),
        None,
    )
    if first_error is not None:
        line += f" first_error={first_error!r}"
    print(line)
    print(f"  -> {path}")


def _connection(base_url: str | None) -> tuple[str, str]:
    """Resolve the base URL and API key, refusing to run without a key."""
    api_key = os.environ.get("TRELIS_API_KEY")
    if not api_key:
        raise CliError(
            "TRELIS_API_KEY is not set; create a key with `trelis admin key create <principal>` "
            "and export it (the key is read from the environment, never a flag)"
        )
    resolved_url = base_url or os.environ.get("TRELIS_BASE_URL") or DEFAULT_BASE_URL
    return resolved_url, api_key


def _print_summary(
    records: Sequence[IngestRecord],
    golden_out: Path,
    manifest_out: Path,
    question_count: int,
) -> None:
    """Print the ingest counts and the two output paths."""
    counts = Counter(record.status for record in records)
    statuses = ", ".join(
        f"{status.value}={counts[status]}"
        for status in (
            IngestStatus.COMPLETED,
            IngestStatus.FAILED,
            IngestStatus.ERROR,
            IngestStatus.TIMEOUT,
        )
    )
    duplicates = sum(1 for record in records if record.duplicate)
    print(f"Ingested {len(records)} Documents: {statuses}, duplicates={duplicates}")
    print(f"Golden Set: {question_count} questions -> {golden_out}")
    print(f"Manifest: {len(records)} records -> {manifest_out}")


def _comma_separated(value: str) -> tuple[str, ...]:
    """Split an argparse comma-separated list, dropping empty names."""
    return tuple(part.strip() for part in value.split(",") if part.strip())


def _positive_int(value: str) -> int:
    """Parse a positive integer argument."""
    number = int(value)
    if number < 1:
        raise argparse.ArgumentTypeError("must be at least 1")
    return number


def _non_negative_int(value: str) -> int:
    """Parse a non-negative integer argument."""
    number = int(value)
    if number < 0:
        raise argparse.ArgumentTypeError("must not be negative")
    return number


def _concurrency(value: str) -> int:
    """Parse --concurrency: how many questions an Ablation Run keeps in flight."""
    number = int(value)
    if not 1 <= number <= MAX_CONCURRENCY:
        raise argparse.ArgumentTypeError(f"must be between 1 and {MAX_CONCURRENCY}")
    return number


def _positive_float(value: str) -> float:
    """Parse a number of seconds that must be above zero."""
    number = float(value)
    if not number > 0:
        raise argparse.ArgumentTypeError("must be greater than 0")
    return number
