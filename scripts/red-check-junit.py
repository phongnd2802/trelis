#!/usr/bin/env python3
"""Classify one pytest run from the JUnit XML report red-check.sh wrote.

Usage:
  python3 -I scripts/red-check-junit.py <xml> <pytest-exit> <log>
  python3 -I scripts/red-check-junit.py --self-test

The first form prints one unit-separator-separated line: verdict, kind,
detail, counts. AssertionError and pytest's own Failed (pytest.fail, or a
failed pytest.raises) are red; every other exception, an <error>, a skip, an
unmatched node id and an unreadable report are not red. A report with several
test cases is red only when every case fails on an assertion.
"""

from __future__ import annotations

import re
import sys
import xml.etree.ElementTree as ET

ASSERTION_TYPES = ("AssertionError", "Failed")

# pytest writes no type attribute on <failure>; the exception class is on the
# reprcrash line ("file:line: ClassName"), or at the start of the message when
# the failure has no traceback text.
_REPRCRASH = re.compile(r"^.*?:\d+: ([A-Z][A-Za-z0-9_.]*)(?::.*)?$")
_MESSAGE_TYPE = re.compile(r"^([A-Za-z_][\w.]*):")


def _crash_type(case: ET.Element) -> tuple[str | None, str]:
    failure = case.find("failure")
    if failure is None:
        return None, ""
    text = failure.text or ""
    for line in reversed([line.strip() for line in text.splitlines() if line.strip()]):
        match = _REPRCRASH.match(line)
        if match:
            return match.group(1), line
    message = failure.get("message", "")
    match = _MESSAGE_TYPE.match(message)
    if match:
        return match.group(1), message
    return None, message


def _plural(count: int, singular: str) -> str:
    return f"{count} {singular}" if count == 1 else f"{count} {singular}s"


def _counts(
    total: int,
    assertions: int,
    others: int,
    unknown: int,
    errors: int,
    skipped: int,
    passed: int,
) -> str:
    if total < 2:
        return ""
    parts = []
    for count, label in (
        (assertions, "assertion failure"),
        (others, "other failure"),
        (unknown, "unknown failure"),
        (errors, "error"),
        (skipped, "skipped test"),
        (passed, "passed test"),
    ):
        if count:
            parts.append(_plural(count, label))
    return f"{total} cases: " + ", ".join(parts)


def classify(report: ET.Element, status: int) -> tuple[str, str, str, str]:
    cases = list(report.iter("testcase"))
    if not cases:
        if status in (4, 5):
            return "NOT RED", "no test", "the node id matched nothing", ""
        return "NOT RED", f"pytest exited {status}", "", ""

    assertions: list[tuple[str, str]] = []
    others: list[tuple[str, str]] = []
    unknown: list[str] = []
    errors: list[str] = []
    skipped: list[str] = []
    passed = 0

    for case in cases:
        error = case.find("error")
        if error is not None:
            errors.append(error.get("message", ""))
            continue
        if case.find("failure") is not None:
            kind, line = _crash_type(case)
            if kind is None:
                unknown.append(line or "a <failure> without an exception type")
            elif kind in ASSERTION_TYPES:
                assertions.append((kind, line))
            else:
                others.append((kind, line))
            continue
        skip = case.find("skipped")
        if skip is not None:
            skipped.append(skip.get("message", ""))
            continue
        passed += 1

    counts = _counts(
        len(cases), len(assertions), len(others), len(unknown), len(errors), len(skipped), passed
    )
    if errors:
        return "NOT RED", "error", errors[0], counts
    if unknown:
        return "NOT RED", "unknown failure type", unknown[0], counts
    if others:
        return "NOT RED", others[0][0], others[0][1], counts
    if skipped:
        return "NOT RED", "skipped", skipped[0], counts
    if passed:
        return "NOT RED", "passed", "", counts
    if len(assertions) == len(cases):
        kinds = sorted({kind for kind, _ in assertions})
        kind = kinds[0] if len(kinds) == 1 else "assertions"
        return "RED", kind, assertions[0][1], counts
    return "NOT RED", "unclassified report", "a test case matched no JUnit state", counts


def classify_text(text: bytes | str, status: int) -> tuple[str, str, str, str]:
    try:
        report = ET.fromstring(text)
    except ET.ParseError:
        return "NOT RED", "unparseable report", "the JUnit XML could not be parsed", ""
    return classify(report, status)


def _log_tail(log_path: str) -> str:
    try:
        with open(log_path, encoding="utf-8", errors="replace") as log:
            lines = [line.strip() for line in log if line.strip()]
    except OSError:
        return ""
    return " | ".join(lines[-2:])


def classify_file(xml_path: str, status: int, log_path: str) -> tuple[str, str, str, str]:
    try:
        with open(xml_path, "rb") as report:
            text = report.read()
    except OSError:
        kind = "usage error" if status == 4 else f"pytest exited {status}"
        return "NOT RED", kind, _log_tail(log_path), ""
    return classify_text(text, status)


SEPARATOR = "\x1f"


def _line(fields: tuple[str, str, str, str]) -> str:
    return SEPARATOR.join(" ".join(str(field).split()) for field in fields)


def _case(name: str, body: str = "") -> str:
    return f'<testcase classname="tests.test_probe" name="{name}">{body}</testcase>'


def _report(*cases: str) -> str:
    return "<testsuites><testsuite>" + "".join(cases) + "</testsuite></testsuites>"


def self_test() -> None:
    checks = [
        (
            "assertion",
            _report(_case("test_a", '<failure message="assert 1 == 2">t.py:3: AssertionError</failure>')),
            1,
            ("RED", "AssertionError", "t.py:3: AssertionError", ""),
        ),
        (
            "pytest.fail",
            _report(_case("test_b", '<failure message="Failed: boom">t.py:4: Failed</failure>')),
            1,
            ("RED", "Failed", "t.py:4: Failed", ""),
        ),
        (
            "other exception",
            _report(
                _case(
                    "test_c",
                    '<failure message="AttributeError: nope">t.py:5: AttributeError</failure>',
                )
            ),
            1,
            ("NOT RED", "AttributeError", "t.py:5: AttributeError", ""),
        ),
        (
            "setup error",
            _report(_case("test_d", '<error message="failed on setup">RuntimeError</error>')),
            1,
            ("NOT RED", "error", "failed on setup", ""),
        ),
        (
            "skip",
            _report(
                _case("test_e", '<skipped type="pytest.skip" message="why">t.py:6: why</skipped>')
            ),
            0,
            ("NOT RED", "skipped", "why", ""),
        ),
        ("passed", _report(_case("test_f")), 0, ("NOT RED", "passed", "", "")),
        (
            "unknown failure type",
            _report(_case("test_g", '<failure message="boom"/>')),
            1,
            ("NOT RED", "unknown failure type", "boom", ""),
        ),
        (
            "unparseable",
            "<testsuites",
            1,
            ("NOT RED", "unparseable report", "the JUnit XML could not be parsed", ""),
        ),
        (
            "no test",
            _report(),
            5,
            ("NOT RED", "no test", "the node id matched nothing", ""),
        ),
        (
            "multi all red",
            _report(
                _case("test_h[1]", '<failure message="assert">t.py:7: AssertionError</failure>'),
                _case("test_h[2]", '<failure message="assert">t.py:7: AssertionError</failure>'),
            ),
            1,
            (
                "RED",
                "AssertionError",
                "t.py:7: AssertionError",
                "2 cases: 2 assertion failures",
            ),
        ),
        (
            "multi mixed",
            _report(
                _case("test_i[1]", '<failure message="assert">t.py:8: AssertionError</failure>'),
                _case("test_i[2]"),
            ),
            1,
            ("NOT RED", "passed", "", "2 cases: 1 assertion failure, 1 passed test"),
        ),
        (
            "multi other failure",
            _report(
                _case("test_j[1]", '<failure message="assert">t.py:9: AssertionError</failure>'),
                _case(
                    "test_j[2]",
                    '<failure message="AttributeError: nope">t.py:9: AttributeError</failure>',
                ),
            ),
            1,
            (
                "NOT RED",
                "AttributeError",
                "t.py:9: AttributeError",
                "2 cases: 1 assertion failure, 1 other failure",
            ),
        ),
    ]
    for name, text, status, expected in checks:
        got = classify_text(text, status)
        assert got == expected, f"{name}: {got!r} != {expected!r}"
    line = _line(("NOT RED", "passed", "", "2 cases: 1 assertion failure, 1 passed test"))
    assert line == "NOT RED\x1fpassed\x1f\x1f2 cases: 1 assertion failure, 1 passed test"


def main(argv: list[str]) -> int:
    if argv == ["--self-test"]:
        self_test()
        print("self-test ok")
        return 0
    if len(argv) != 3:
        print(
            "usage: red-check-junit.py <xml> <pytest-exit> <log> | --self-test",
            file=sys.stderr,
        )
        return 64
    xml_path, status, log_path = argv
    try:
        exit_code = int(status)
    except ValueError:
        print("red-check-junit.py: pytest exit is not an integer", file=sys.stderr)
        return 64
    print(_line(classify_file(xml_path, exit_code, log_path)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
