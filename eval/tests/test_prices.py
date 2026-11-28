"""Tests for the price table and one answer's generate cost."""

from __future__ import annotations

from pathlib import Path

import pytest

from trelis_eval.client import TraceStep, Usage
from trelis_eval.prices import (
    ModelPrice,
    PriceTable,
    generate_model,
    load_prices,
    question_cost,
)

CHAT = "deepseek-v4-flash-0731"


def write_prices(tmp_path, text):
    path = tmp_path / "prices.toml"
    path.write_text(text, encoding="utf-8")
    return path


def test_load_prices_parses_the_currency_and_each_model(tmp_path):
    path = write_prices(
        tmp_path,
        """
currency = "USD"

[models."deepseek-v4-flash-0731"]
input_per_million = 0.27
output_per_million = 1.10

[models."other"]
input_per_million = 2
output_per_million = 3
""",
    )

    prices = load_prices(path)

    assert prices.currency == "USD"
    assert prices.models == {
        "deepseek-v4-flash-0731": ModelPrice(input_per_million=0.27, output_per_million=1.10),
        "other": ModelPrice(input_per_million=2.0, output_per_million=3.0),
    }


def test_load_prices_collects_every_problem_in_one_error(tmp_path):
    path = write_prices(
        tmp_path,
        """
extra = 1

[models."chat-x"]
input_per_million = -1
output_per_million = "free"
unknown_field = true

[models."chat-y"]
input_per_million = 1
""",
    )

    with pytest.raises(ValueError) as exc_info:
        load_prices(path)

    message = str(exc_info.value)
    for part in (str(path), "currency", "extra", "chat-x", "unknown_field"):
        assert part in message
    assert f"{path}: models['chat-x']: 'input_per_million' must not be negative" in message
    assert f"{path}: models['chat-x']: 'output_per_million' must be a number" in message
    assert f"{path}: models['chat-y']: missing 'output_per_million'" in message


def test_a_negative_price_is_rejected(tmp_path):
    path = write_prices(
        tmp_path,
        """
currency = "USD"

[models."chat-x"]
input_per_million = -0.1
output_per_million = 1
""",
    )

    with pytest.raises(ValueError, match="input_per_million"):
        load_prices(path)


def test_the_committed_template_loads_with_no_models():
    template = Path(__file__).parent.parent / "prices.toml"

    prices = load_prices(template)

    assert prices.currency == "USD"
    assert prices.models == {}


def test_generate_model_reads_the_generate_trace_step():
    trace = (
        TraceStep(
            step="embed_query",
            technique="dense",
            duration_ms=4,
            model="embed-x",
            candidates=None,
        ),
        TraceStep(step="generate", technique=None, duration_ms=900, model=CHAT, candidates=None),
    )

    assert generate_model(trace) == CHAT
    assert generate_model(()) is None


def test_question_cost_is_priced_by_the_generate_steps_model():
    prices = PriceTable(
        currency="USD",
        models={CHAT: ModelPrice(input_per_million=0.27, output_per_million=1.10)},
    )
    trace = (
        TraceStep(
            step="embed_query",
            technique="dense",
            duration_ms=4,
            model="embed-x",
            candidates=None,
        ),
        TraceStep(step="generate", technique=None, duration_ms=900, model=CHAT, candidates=None),
    )
    usage = Usage(prompt_tokens=1000, completion_tokens=500, total_tokens=1500)

    assert question_cost(usage, trace, prices) == pytest.approx(0.00027 + 0.00055)


def test_zero_usage_costs_zero_even_without_a_price():
    prices = PriceTable(currency="USD", models={})
    usage = Usage(prompt_tokens=0, completion_tokens=0, total_tokens=0)

    assert question_cost(usage, (), prices) == 0.0


def test_a_model_without_a_price_has_no_cost():
    prices = PriceTable(
        currency="USD",
        models={CHAT: ModelPrice(input_per_million=1.0, output_per_million=1.0)},
    )
    trace = (
        TraceStep(step="generate", technique=None, duration_ms=1, model="other", candidates=None),
    )
    usage = Usage(prompt_tokens=1, completion_tokens=1, total_tokens=2)

    assert question_cost(usage, trace, prices) is None
