"""Every translation carries every string strings.json defines.

The French file went without the LAN streaming option for a release, which
leaves the options form with a raw key for a label.
"""
import json
from pathlib import Path

import pytest

ROOT = Path(__file__).parent.parent.parent / "custom_components" / "philips_avent"


def _keys(tree: dict, prefix: str = "") -> set[str]:
    out = set()
    for key, value in tree.items():
        path = f"{prefix}.{key}" if prefix else key
        out |= _keys(value, path) if isinstance(value, dict) else {path}
    return out


@pytest.mark.parametrize("path", sorted((ROOT / "translations").glob("*.json")), ids=lambda p: p.name)
def test_translation_matches_strings(path):
    expected = _keys(json.loads((ROOT / "strings.json").read_text()))
    actual = _keys(json.loads(path.read_text()))
    assert sorted(expected - actual) == [], "missing"
    assert sorted(actual - expected) == [], "extra"
