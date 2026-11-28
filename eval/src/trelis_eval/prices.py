"""The price table that prices an answer's generate call.

Prices are configuration, not code: ``eval/prices.toml`` holds the provider's
per-million-token prices for the models Trelis reports in a question's
``generate`` trace step, each with its source and the date it was read.
``eval/docs/metrics.md`` defines Cost per Correct Answer.
"""

from __future__ import annotations

import tomllib
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .client import TraceStep, Usage

_PRICE_FIELDS = ("input_per_million", "output_per_million")
_TOP_LEVEL_FIELDS = {"currency", "models"}
_TOKENS_PER_MILLION = 1_000_000


@dataclass(frozen=True)
class ModelPrice:
    """One model's input and output price per million tokens."""

    input_per_million: float
    output_per_million: float


@dataclass(frozen=True)
class PriceTable:
    """A currency and the price of each model named by a generate step."""

    currency: str
    models: Mapping[str, ModelPrice]


def load_prices(path: str | Path) -> PriceTable:
    """Load a price table, collecting every problem in one error."""
    path = Path(path)
    if not path.is_file():
        raise ValueError(f"{path}: no price table")
    try:
        with path.open("rb") as source:
            value = tomllib.load(source)
    except tomllib.TOMLDecodeError as exc:
        raise ValueError(f"{path}: invalid TOML: {exc}") from exc
    return _parse_prices(value, path)


def _parse_prices(value: dict[str, Any], path: Path) -> PriceTable:
    """Validate one decoded price table, appending every problem it has."""
    problems: list[str] = []

    def add(message: str) -> None:
        problems.append(f"{path}: {message}")

    unknown = sorted(set(value) - _TOP_LEVEL_FIELDS)
    if unknown:
        add(f"unknown key(s): {', '.join(repr(name) for name in unknown)}")

    currency = value.get("currency")
    if not isinstance(currency, str) or not currency.strip():
        add("'currency' must be a non-empty string")
        currency = ""

    raw_models = value.get("models", {})
    if not isinstance(raw_models, dict):
        add("'models' must be a table of model names")
        raw_models = {}

    models: dict[str, ModelPrice] = {}
    for name, entry in raw_models.items():
        price = _parse_model_price(name, entry, add)
        if price is not None:
            models[name] = price

    if problems:
        raise ValueError("\n".join(problems))
    return PriceTable(currency=currency, models=models)


def _parse_model_price(name: str, entry: Any, add: Callable[[str], None]) -> ModelPrice | None:
    """Validate one model's price entry, or record why it is invalid."""
    where = f"models[{name!r}]"
    if not isinstance(entry, dict):
        add(f"{where} must be a table")
        return None

    unknown = sorted(set(entry) - set(_PRICE_FIELDS))
    if unknown:
        add(f"{where}: unknown key(s): {', '.join(repr(key) for key in unknown)}")

    values: list[float | None] = []
    for field in _PRICE_FIELDS:
        if field not in entry:
            add(f"{where}: missing {field!r}")
            values.append(None)
            continue
        raw = entry[field]
        if not isinstance(raw, (int, float)) or isinstance(raw, bool):
            add(f"{where}: {field!r} must be a number")
            values.append(None)
        elif raw < 0:
            add(f"{where}: {field!r} must not be negative")
            values.append(None)
        else:
            values.append(float(raw))

    input_price, output_price = values
    if input_price is None or output_price is None:
        return None
    return ModelPrice(input_per_million=input_price, output_per_million=output_price)


def generate_model(trace: Sequence[TraceStep]) -> str | None:
    """Return the model of a question's generate trace step, if it has one."""
    return next((step.model for step in trace if step.step == "generate"), None)


def question_cost(usage: Usage, trace: Sequence[TraceStep], prices: PriceTable) -> float | None:
    """Price one answer's generate call, or ``None`` when its model has no price.

    Only the chat call's usage is priced, because Trelis's debug object reports
    only that call. A question whose usage is all zeros made no chat call, so
    it costs zero without needing a price.
    """
    if usage.prompt_tokens == 0 and usage.completion_tokens == 0 and usage.total_tokens == 0:
        return 0.0
    model = generate_model(trace)
    price = prices.models.get(model) if model is not None else None
    if price is None:
        return None
    return (
        usage.prompt_tokens * price.input_per_million / _TOKENS_PER_MILLION
        + usage.completion_tokens * price.output_per_million / _TOKENS_PER_MILLION
    )
