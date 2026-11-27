"""Ingest a selected Corpus through the public API and wait for its Documents."""

from __future__ import annotations

import json
import time
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path

import httpx

from .client import TrelisClient, TrelisError
from .multihop import BenchmarkDocument, document_markdown


class IngestStatus(StrEnum):
    """The final state of one ingest record."""

    PENDING = "pending"
    PROCESSING = "processing"
    COMPLETED = "completed"
    FAILED = "failed"
    TIMEOUT = "timeout"
    ERROR = "error"


@dataclass
class IngestRecord:
    """One uploaded Benchmark Document and the final state of its Document."""

    source_id: str
    document_id: str | None
    file_name: str
    status: IngestStatus
    duplicate: bool = False
    error: str = ""


def ingest(
    client: TrelisClient,
    documents: Sequence[BenchmarkDocument],
    principals: Sequence[str] = (),
    groups: Sequence[str] = (),
    poll_interval: float = 1.0,
    timeout: float = 300.0,
    *,
    clock: Callable[[], float] = time.monotonic,
    sleep: Callable[[float], None] = time.sleep,
) -> list[IngestRecord]:
    """Upload every Benchmark Document, then poll until each Document ends.

    The returned records are in the order the Benchmark Documents were given.
    An upload that fails is recorded as ``error`` and the run continues. A
    failed poll leaves that record pending and is retried next round; it is
    recorded as ``timeout`` if it is still pending at the deadline.
    """
    records: list[IngestRecord] = []
    for benchmark_document in documents:
        file_name, content = document_markdown(benchmark_document)
        try:
            uploaded = client.upload_document(file_name, content, principals, groups)
        except (TrelisError, httpx.HTTPError) as exc:
            records.append(
                IngestRecord(
                    source_id=benchmark_document.source_id,
                    document_id=None,
                    file_name=file_name,
                    status=IngestStatus.ERROR,
                    error=str(exc),
                )
            )
            continue
        document = uploaded.document
        status = IngestStatus(document.parse_status)
        records.append(
            IngestRecord(
                source_id=benchmark_document.source_id,
                document_id=document.id,
                file_name=file_name,
                status=status,
                duplicate=uploaded.duplicate,
                error=document.parse_error if status == IngestStatus.FAILED else "",
            )
        )

    pending = [
        record
        for record in records
        if record.status in (IngestStatus.PENDING, IngestStatus.PROCESSING)
    ]
    started = clock()
    while pending:
        for record in list(pending):
            try:
                document = client.get_document(record.document_id)
            except (TrelisError, httpx.HTTPError) as exc:
                record.error = str(exc)
                continue
            record.status = IngestStatus(document.parse_status)
            record.error = (
                document.parse_error if record.status == IngestStatus.FAILED else ""
            )
            if record.status in (IngestStatus.COMPLETED, IngestStatus.FAILED):
                pending.remove(record)
        if not pending:
            break
        if clock() - started >= timeout:
            for record in pending:
                previous = record.status
                last_error = record.error
                record.status = IngestStatus.TIMEOUT
                record.error = f"still {previous.value} after {timeout:g}s"
                if last_error:
                    record.error += f"; last poll error: {last_error}"
            break
        sleep(poll_interval)

    return records


def write_manifest(path: str | Path, records: Iterable[IngestRecord]) -> None:
    """Write one JSON object per IngestRecord, mapping source_id to document_id."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as out:
        for record in records:
            json.dump(
                {
                    "source_id": record.source_id,
                    "document_id": record.document_id,
                    "file_name": record.file_name,
                    "status": record.status.value,
                    "duplicate": record.duplicate,
                    "error": record.error,
                },
                out,
                ensure_ascii=False,
            )
            out.write("\n")