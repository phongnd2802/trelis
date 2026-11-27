"""Tests for the MultiHop-RAG loader, subset selection and upload bodies."""

import json
import random
from dataclasses import replace
from pathlib import Path

import httpx
import pytest

from trelis_eval.golden import Evidence, GoldenQuestion
from trelis_eval.multihop import (
    DATA_BASE_URL,
    BenchmarkDocument,
    MultiHopDataError,
    document_markdown,
    fetch,
    load_corpus,
    load_questions,
    select_subset,
    stratified_sample,
    to_golden_set,
)

FIXTURES = Path(__file__).parent / "fixtures"


def test_load_corpus_reads_every_document():
    documents = load_corpus(FIXTURES / "corpus.json")

    assert [document.source_id for document in documents] == [
        "https://example.com/alpha",
        "https://example.com/beta",
        "https://example.com/gamma",
        "https://example.com/delta",
    ]
    alpha = documents[0]
    assert alpha == BenchmarkDocument(
        source_id="https://example.com/alpha",
        title="Alpha Mission Launches",
        author="Ada Lovelace",
        source="Example Science",
        published_at="2024-01-01T10:00:00+00:00",
        category="science",
        body="The Alpha Mission launched on 1 January 2024.\n\nIt carries a small telescope.",
    )
    assert documents[1].author is None


def test_load_questions_reads_evidence_and_a_null_query():
    questions = load_questions(FIXTURES / "MultiHopRAG.json")

    assert len(questions) == 4
    assert questions[0].question_type == "comparison_query"
    assert [evidence.url for evidence in questions[0].evidence] == [
        "https://example.com/alpha",
        "https://example.com/beta",
    ]
    assert questions[2].question_type == "null_query"
    assert questions[2].evidence == ()


def make_stratified_pool(counts):
    """Build one Golden question per count, in a stable id order."""
    golden = []
    index = 0
    for question_type, count in counts.items():
        for _ in range(count):
            golden.append(
                GoldenQuestion(
                    id=f"q-{index:05d}",
                    question=f"Question {index}?",
                    expected_answer="A",
                    evidence=(Evidence(source_id=f"https://example.com/{index}", fact="f"),),
                    slices=(question_type, "hops-1"),
                )
            )
            index += 1
    return golden


def question_types(questions):
    return sorted(
        slice_name
        for question in questions
        for slice_name in question.slices
        if not slice_name.startswith("hops-")
    )


def test_stratified_sample_follows_the_multihop_rag_question_type_split():
    # The published 2,556 questions split 856 / 816 / 583 / 301; the largest
    # remainder method gives 167 / 160 / 114 / 59 for a 500-question sample.
    golden = make_stratified_pool(
        {
            "comparison_query": 856,
            "inference_query": 816,
            "temporal_query": 583,
            "null_query": 301,
        }
    )

    sample = stratified_sample(golden, 500, seed=1)

    assert len(sample) == 500
    counts = {
        name: question_types(sample).count(name)
        for name in (
            "comparison_query",
            "inference_query",
            "temporal_query",
            "null_query",
        )
    }
    assert counts == {
        "comparison_query": 167,
        "inference_query": 160,
        "temporal_query": 114,
        "null_query": 59,
    }
    assert [question.id for question in sample] == sorted(question.id for question in sample)


def test_stratified_sample_breaks_remainder_ties_by_slice_name():
    golden = make_stratified_pool({"d_type": 1, "b_type": 1, "a_type": 1, "c_type": 1})

    sample = stratified_sample(golden, 2, seed=4)

    assert question_types(sample) == ["a_type", "b_type"]


def test_stratified_sample_is_deterministic_for_a_seed():
    golden = make_stratified_pool({"comparison_query": 20, "inference_query": 20})

    first = stratified_sample(golden, 10, seed=7)
    second = stratified_sample(golden, 10, seed=7)

    assert [question.id for question in first] == [question.id for question in second]
    assert len({question.id for question in first}) == 10


def test_stratified_sample_uses_one_random_stream_across_slices():
    golden = make_stratified_pool({"a_type": 3, "b_type": 3})
    rng = random.Random(5)
    expected = rng.sample(["q-00000", "q-00001", "q-00002"], 2) + rng.sample(
        ["q-00003", "q-00004", "q-00005"], 2
    )

    sample = stratified_sample(golden, 4, seed=5)

    assert [question.id for question in sample] == sorted(expected)


def test_stratified_sample_rejects_a_sample_larger_than_the_pool():
    golden = make_stratified_pool({"comparison_query": 2, "inference_query": 1})

    with pytest.raises(ValueError, match="only 3"):
        stratified_sample(golden, 4, seed=1)


def test_stratified_sample_rejects_a_question_without_one_question_type_slice():
    no_type = GoldenQuestion(
        id="q1",
        question="Who?",
        expected_answer="A",
        evidence=(),
        slices=("hops-0",),
    )
    two_types = replace(no_type, id="q2", slices=("comparison_query", "inference_query"))

    for question in (no_type, two_types):
        with pytest.raises(ValueError, match=question.id):
            stratified_sample([question], 1, seed=1)


def test_stratified_sample_of_zero_questions_is_empty():
    golden = make_stratified_pool({"comparison_query": 1})

    assert stratified_sample(golden, 0, seed=1) == []


def test_load_corpus_names_the_file_index_and_field(tmp_path):
    data = json.loads((FIXTURES / "corpus.json").read_text(encoding="utf-8"))
    del data[1]["body"]
    path = tmp_path / "corpus.json"
    path.write_text(json.dumps(data), encoding="utf-8")

    with pytest.raises(MultiHopDataError) as caught:
        load_corpus(path)

    message = str(caught.value)
    assert str(path) in message
    assert "document 1" in message
    assert "'body'" in message


def test_load_corpus_rejects_mistyped_author(tmp_path):
    data = json.loads((FIXTURES / "corpus.json").read_text(encoding="utf-8"))
    data[0]["author"] = 7
    path = tmp_path / "corpus.json"
    path.write_text(json.dumps(data), encoding="utf-8")

    with pytest.raises(MultiHopDataError, match=r"document 0.*'author'"):
        load_corpus(path)


def test_load_corpus_rejects_duplicate_urls(tmp_path):
    data = json.loads((FIXTURES / "corpus.json").read_text(encoding="utf-8"))
    data[1]["url"] = data[0]["url"]
    path = tmp_path / "corpus.json"
    path.write_text(json.dumps(data), encoding="utf-8")

    with pytest.raises(MultiHopDataError, match=r"document 1.*duplicate url"):
        load_corpus(path)


def test_load_questions_rejects_an_unknown_question_type(tmp_path):
    data = json.loads((FIXTURES / "MultiHopRAG.json").read_text(encoding="utf-8"))
    data[0]["question_type"] = "bogus_query"
    path = tmp_path / "MultiHopRAG.json"
    path.write_text(json.dumps(data), encoding="utf-8")

    with pytest.raises(MultiHopDataError, match=r"question 0.*'question_type'"):
        load_questions(path)


def test_load_questions_names_a_bad_evidence_field(tmp_path):
    data = json.loads((FIXTURES / "MultiHopRAG.json").read_text(encoding="utf-8"))
    del data[0]["evidence_list"][1]["fact"]
    path = tmp_path / "MultiHopRAG.json"
    path.write_text(json.dumps(data), encoding="utf-8")

    with pytest.raises(MultiHopDataError, match=r"question 0.*evidence 1.*'fact'"):
        load_questions(path)


def test_to_golden_set_maps_ids_evidence_and_slices():
    golden = to_golden_set(load_questions(FIXTURES / "MultiHopRAG.json"))

    assert [question.id for question in golden] == ["mhr-0000", "mhr-0001", "mhr-0002", "mhr-0003"]
    comparison, inference, null_query, temporal = golden
    assert comparison.slices == ("comparison_query", "hops-2")
    assert [evidence.source_id for evidence in comparison.evidence] == [
        "https://example.com/alpha",
        "https://example.com/beta",
    ]
    assert comparison.evidence[0].fact == "The Alpha Mission launched in 2024."
    assert comparison.principal is None
    assert inference.slices == ("inference_query", "hops-2")
    assert len(inference.evidence) == 3
    assert null_query.slices == ("null_query", "hops-0")
    assert null_query.evidence == ()
    assert temporal.slices == ("temporal_query", "hops-1")


def test_select_subset_keeps_evidence_documents_and_adds_distractors():
    documents = load_corpus(FIXTURES / "corpus.json")
    golden = to_golden_set(load_questions(FIXTURES / "MultiHopRAG.json"))

    questions, selected = select_subset(documents, golden, questions=2, extra_docs=1, seed=7)

    assert questions == golden[:2]
    source_ids = [document.source_id for document in selected]
    assert source_ids[:3] == [
        "https://example.com/alpha",
        "https://example.com/beta",
        "https://example.com/gamma",
    ]
    assert len(source_ids) == 4
    assert source_ids[3] == "https://example.com/delta"


def test_select_subset_is_deterministic_for_a_seed():
    documents = load_corpus(FIXTURES / "corpus.json")
    golden = to_golden_set(load_questions(FIXTURES / "MultiHopRAG.json"))

    first_questions, first_documents = select_subset(documents, golden, 1, 1, seed=11)
    second_questions, second_documents = select_subset(documents, golden, 1, 1, seed=11)

    assert first_questions == second_questions
    assert first_documents == second_documents
    assert len(first_documents) == 3


def test_select_subset_rejects_evidence_missing_from_the_corpus():
    documents = load_corpus(FIXTURES / "corpus.json")
    golden = [
        GoldenQuestion(
            id="q1",
            question="Who?",
            expected_answer="A",
            evidence=(Evidence(source_id="https://example.com/missing", fact="f"),),
            slices=("hops-1",),
        )
    ]

    with pytest.raises(ValueError, match="https://example.com/missing"):
        select_subset(documents, golden, 1, 0, seed=0)


def test_select_subset_rejects_more_distractors_than_available():
    documents = load_corpus(FIXTURES / "corpus.json")
    golden = to_golden_set(load_questions(FIXTURES / "MultiHopRAG.json"))

    with pytest.raises(ValueError, match="not enough"):
        select_subset(documents, golden, 3, 10, seed=0)


def test_document_markdown_is_deterministic_and_names_a_safe_file():
    alpha = load_corpus(FIXTURES / "corpus.json")[0]

    first_name, first_bytes = document_markdown(alpha)
    second_name, second_bytes = document_markdown(alpha)

    assert (first_name, first_bytes) == (second_name, second_bytes)
    assert first_name.endswith(".md")
    assert first_name == first_name.lower()
    assert len(first_name) < 100
    assert all(character.isalnum() or character in "-_." for character in first_name)
    text = first_bytes.decode("utf-8")
    assert text.startswith("# Alpha Mission Launches\n")
    assert "- Source: Example Science" in text
    assert "- Published: 2024-01-01T10:00:00+00:00" in text
    assert "- Category: science" in text
    assert "- URL: https://example.com/alpha" in text
    assert "It carries a small telescope." in text


def test_document_markdown_uses_the_url_to_keep_same_titles_apart():
    alpha = load_corpus(FIXTURES / "corpus.json")[0]
    copy = replace(alpha, source_id="https://example.com/another-alpha")

    assert document_markdown(alpha)[0] != document_markdown(copy)[0]


def test_document_markdown_slugs_a_non_latin_title():
    document = BenchmarkDocument(
        source_id="https://example.com/vn",
        title="Bộ Tài chính — Báo cáo",
        author=None,
        source="Example",
        published_at="2024-01-01",
        category="law",
        body="Nội dung.",
    )

    file_name, _ = document_markdown(document)

    assert file_name.endswith(".md")
    assert file_name.startswith("bo-tai-chinh-bao-cao-")


def test_document_markdown_falls_back_for_a_title_with_no_ascii():
    document = BenchmarkDocument(
        source_id="https://example.com/jp",
        title="東京の報告",
        author=None,
        source="Example",
        published_at="2024-01-01",
        category="news",
        body="本文。",
    )

    file_name, _ = document_markdown(document)

    assert file_name.startswith("document-")
    assert file_name.endswith(".md")


def test_fetch_downloads_missing_files_and_skips_present_ones(tmp_path):
    requests = []

    def handle(request):
        requests.append(str(request.url))
        return httpx.Response(200, content=b'{"downloaded": true}')

    client = httpx.Client(transport=httpx.MockTransport(handle))
    first = fetch(tmp_path, client=client)
    second = fetch(
        tmp_path,
        client=httpx.Client(
            transport=httpx.MockTransport(lambda request: pytest.fail("fetched twice"))
        ),
    )

    assert [path.name for path in first] == ["corpus.json", "MultiHopRAG.json"]
    assert [path.name for path in second] == ["corpus.json", "MultiHopRAG.json"]
    assert requests == [
        f"{DATA_BASE_URL}/corpus.json",
        f"{DATA_BASE_URL}/MultiHopRAG.json",
    ]
    assert (tmp_path / "corpus.json").read_bytes() == b'{"downloaded": true}'
