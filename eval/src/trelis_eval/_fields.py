"""Shared JSON field validation for the eval harness.

These helpers raise ``ValueError`` with an optional ``where`` prefix, so a
results-file reader can name the path and line while an API payload reader
leaves it out. ``golden.py`` keeps its own collected-error validation.
"""

from __future__ import annotations

from typing import Any


def check_fields(
    value: dict[str, Any],
    allowed: set[str],
    where: str,
    *,
    required: set[str] | None = None,
) -> None:
    """Reject an object with undeclared or missing fields."""
    unknown = sorted(set(value) - allowed)
    if unknown:
        raise ValueError(
            _at(where, f"unknown field(s): {', '.join(repr(name) for name in unknown)}")
        )
    missing = sorted((allowed if required is None else required) - set(value))
    if missing:
        raise ValueError(
            _at(where, f"missing field(s): {', '.join(repr(name) for name in missing)}")
        )


def string_field(value: dict[str, Any], name: str, where: str = "") -> str:
    """Return a required string field."""
    field = value.get(name)
    if not isinstance(field, str):
        raise ValueError(_at(where, f"field {name!r} must be a string"))
    return field


def optional_string_field(value: dict[str, Any], name: str, where: str = "") -> str | None:
    """Return an optional string field, which may be absent or null."""
    field = value.get(name)
    if field is None or isinstance(field, str):
        return field
    raise ValueError(_at(where, f"field {name!r} must be a string or null"))


def int_field(value: dict[str, Any], name: str, where: str = "") -> int:
    """Return a required integer field, rejecting booleans and floats."""
    field = value.get(name)
    if not isinstance(field, int) or isinstance(field, bool):
        raise ValueError(_at(where, f"field {name!r} must be an integer"))
    return field


def optional_int_field(value: dict[str, Any], name: str, where: str = "") -> int | None:
    """Return an optional integer field, which may be absent or null."""
    field = value.get(name)
    if field is None:
        return None
    if not isinstance(field, int) or isinstance(field, bool):
        raise ValueError(_at(where, f"field {name!r} must be an integer or null"))
    return field


def float_field(value: dict[str, Any], name: str, where: str = "") -> float:
    """Return a required number field as a float, rejecting booleans."""
    field = value.get(name)
    if not isinstance(field, (int, float)) or isinstance(field, bool):
        raise ValueError(_at(where, f"field {name!r} must be a number"))
    return float(field)


def optional_float_field(value: dict[str, Any], name: str, where: str = "") -> float | None:
    """Return an optional number field as a float, allowing null."""
    field = value.get(name)
    if field is None:
        return None
    if not isinstance(field, (int, float)) or isinstance(field, bool):
        raise ValueError(_at(where, f"field {name!r} must be a number or null"))
    return float(field)


def string_list_field(value: dict[str, Any], name: str, where: str = "") -> tuple[str, ...]:
    """Return a required list of strings."""
    field = value.get(name)
    if not isinstance(field, list) or not all(isinstance(item, str) for item in field):
        raise ValueError(_at(where, f"field {name!r} must be a list of strings"))
    return tuple(field)


def float_map_field(value: dict[str, Any], name: str, where: str = "") -> dict[str, float]:
    """Return a required object mapping names to numbers."""
    field = value.get(name)
    if not isinstance(field, dict):
        raise ValueError(_at(where, f"field {name!r} must be an object"))
    result: dict[str, float] = {}
    for key, item in field.items():
        if not isinstance(key, str):
            raise ValueError(_at(where, f"field {name!r} must map technique names to numbers"))
        if not isinstance(item, (int, float)) or isinstance(item, bool):
            raise ValueError(_at(where, f"field {name!r}[{key!r}] must be a number"))
        result[key] = float(item)
    return result


def _at(where: str, message: str) -> str:
    """Prefix a message with a location when one is known."""
    return f"{where}: {message}" if where else message
