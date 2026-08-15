"""Make the development scripts under tools/ importable."""
import sys
from pathlib import Path

_tools_dir = str(Path(__file__).parent.parent.parent / "tools")
if _tools_dir not in sys.path:
    sys.path.insert(0, _tools_dir)
