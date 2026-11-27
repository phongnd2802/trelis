"""The trelis-eval command line."""

from __future__ import annotations

import argparse
import os
import sys
from collections import Counter
from collections.abc import Sequence
from pathlib import Path

import httpx

from .client import TrelisClient, TrelisError
from .golden import GoldenSetError, load_golden_set, write_golden_set
from .ingest import IngestRecord, IngestStatus, ingest, write_manifest
from .multihop import (
    MultiHopDataError,
    fetch,
    load_corpus,
    load_questions,
    select_subset,
    to_golden_set,
)

DEFAULT_DATA_DIR = Path("eval/data/multihop-rag")
DEFAULT_BASE_URL = "http://localhost:8080"


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
    ingest_command.add_argument("--seed", type=int, default=0, help="distractor selection seed")
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
