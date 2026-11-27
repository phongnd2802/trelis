# Golden Set format

A Golden Set is a fixed list of questions, each with its expected answer and
the evidence that supports it, against which Trelis is scored. The harness
stores one in **JSONL**: one JSON object per line, UTF-8, no blank lines.
Every line is one Golden question.

The MultiHop-RAG Benchmark Corpus is converted to this format by the harness;
hand-written Golden Sets use the same schema. MultiHop-RAG calls the entries
of `corpus.json` articles; this harness calls each one a Benchmark Document.

## Fields

| Field | Type | Required | Meaning |
|---|---|---|---|
| `id` | string | yes | Unique within the file. Stable across runs; MultiHop-RAG uses `mhr-0000`, `mhr-0001`, … by position in `MultiHopRAG.json`. |
| `question` | string | yes | The question asked of Trelis. Non-empty. |
| `expected_answer` | string | yes | The reference answer. May be empty. |
| `evidence` | list of objects | yes | Facts that support the answer. May be empty (for example a null query). Each object has exactly `source_id` and `fact`, both non-empty strings. |
| `slices` | list of strings | yes | The Slices this question belongs to. Every item is a non-empty string. |
| `principal` | string | no | The Principal the question is asked as. Absent means the run's default Principal. Used later for adversarial access questions. |

`evidence[].source_id` is the URL of the MultiHop-RAG document in a Golden
Set, and the manifest written by `trelis-eval multihop ingest` maps it to the
Trelis Document ID.

## Example line

```json
{"id": "mhr-0000", "question": "Which missions launched or returned in 2024?", "expected_answer": "Alpha and Beta.", "evidence": [{"source_id": "https://example.com/alpha", "fact": "The Alpha Mission launched in 2024."}], "slices": ["comparison_query", "hops-1"]}
```

Writing a Golden Set produces exactly this shape; loading it and writing it
again is lossless.

## Validation

`trelis-eval golden validate PATH` loads the file and rejects it as a whole
when any line is wrong:

- the line is valid JSON and decodes to an object;
- no blank lines;
- all required fields are present and no unknown fields appear, at the top
  level or inside an `evidence` item;
- fields have the types in the table above; `id` and `question` are non-empty,
  and each `evidence` item has non-empty `source_id` and `fact`;
- `id`s are unique across the file;
- `slices` is a list and every item is a non-empty string; `evidence` is a
  list and may be empty;
- `principal`, when present, is a non-empty string.

Every problem is collected before failing, and each error is reported as
`path:line: message`, so one run fixes the whole file. The command exits 1 on
an invalid file and prints Slice counts on a valid one.

## MultiHop-RAG mapping

`trelis-eval multihop golden` converts every question in `MultiHopRAG.json`:

- `id` is `mhr-NNNN`, zero-padded and zero-based, by position in the file.
- `question` is `query`; `expected_answer` is `answer`.
- `evidence` has one item per entry in `evidence_list`; `source_id` is the
  evidence URL and `fact` is its `fact`. Several facts may cite the same
  document, so the item count can exceed the number of documents.
- `slices` always contains `hops-N`, where N is the number of **distinct**
  evidence URLs. A `null_query` has an empty `evidence_list`, so it gets
  `hops-0`. When `question_type` is present it is added as a Slice too, e.g.
  `comparison_query`; a question whose `question_type` is `null` gets only the
  `hops-N` Slice.
- `principal` is omitted: the question is asked as the run's default
  Principal.

`trelis-eval multihop ingest` writes the same questions, restricted to the
selected subset and with the same IDs, so the full Golden Set and a run's
subset stay comparable.