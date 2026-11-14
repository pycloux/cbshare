"""The suite and cases.yaml must agree, so the plan never claims a case is
automated when it is not."""

import re
from pathlib import Path

import pytest

from conftest import CASES

HERE = Path(__file__).parent
MARK = re.compile(r'@pytest\.mark\.case\("([A-Z]+-\d+)"\)')


def declared_cases() -> dict[str, list[str]]:
    found: dict[str, list[str]] = {}
    for f in sorted(HERE.glob("test_*.py")):
        for cid in MARK.findall(f.read_text()):
            found.setdefault(cid, []).append(f.name)
    return found


@pytest.mark.p1
def test_every_automated_case_has_a_test():
    found = declared_cases()
    missing = [cid for cid, c in CASES.items() if c["automation"] == "automated" and cid not in found]
    assert not missing, f"marked 'automated' in cases.yaml but no test: {missing}"


@pytest.mark.p1
def test_every_test_names_an_automated_case():
    found = declared_cases()
    wrong = {cid: files for cid, files in found.items()
             if cid not in CASES or CASES[cid]["automation"] != "automated"}
    assert not wrong, f"tests for cases that are unknown or not 'automated' in cases.yaml: {wrong}"


@pytest.mark.p1
def test_cases_are_complete():
    for cid, c in CASES.items():
        assert c.get("title") and c.get("priority") in ("P1", "P2", "P3"), cid
        assert c.get("automation") in ("automated", "planned", "manual", "suite"), cid
        if c["automation"] == "manual":
            assert c.get("reason"), f"{cid}: manual cases need a reason"
