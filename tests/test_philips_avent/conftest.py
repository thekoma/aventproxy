"""Make philips_avent modules importable without homeassistant."""
import sys
from pathlib import Path

# Add the philips_avent directory directly to sys.path
# so api.py and const.py can be imported without triggering __init__.py
_pkg_dir = str(Path(__file__).parent.parent.parent / "custom_components" / "philips_avent")
if _pkg_dir not in sys.path:
    sys.path.insert(0, _pkg_dir)
