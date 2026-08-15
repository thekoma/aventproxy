"""Credential loading for the development scripts in this directory.

These scripts talk to a live Tuya account: they need a session id, an ecode, a
partner identity and sometimes a camera's local key. Those values must never be
written into a script, because everything in ``tools/`` is committed. A real
sid in the repository is a live account takeover, and history keeps it long
after the line is deleted.

So: values come from the environment, or from ``tools/credentials.json``, which
``.gitignore`` excludes. There are no defaults and no fallbacks — a missing
value is an error with instructions, never a silent empty string.

Environment variable names are the credential name upper-cased with an
``AVENT_`` prefix: ``local_key`` reads ``AVENT_LOCAL_KEY``.
"""
from __future__ import annotations

import json
import os
from pathlib import Path

ENV_PREFIX = "AVENT_"
CREDENTIALS_FILE = Path(__file__).parent / "credentials.json"


class MissingCredentials(RuntimeError):
    """Raised when a required credential was not supplied anywhere."""


def mask(value: str, keep: int = 4) -> str:
    """Render a secret for the console: a short prefix, the rest hidden.

    Enough to tell two sessions apart in a log, not enough to reuse.
    """
    if len(value) <= keep:
        return "*" * len(value)
    return f"{value[:keep]}{'*' * (len(value) - keep)}"


def env_var_for(name: str) -> str:
    """Environment variable a credential is read from."""
    return f"{ENV_PREFIX}{name.upper()}"


def _from_file(path: Path) -> dict:
    if not path.exists():
        return {}
    try:
        loaded = json.loads(path.read_text())
    except (OSError, ValueError) as err:
        raise MissingCredentials(f"Could not read credentials from {path}: {err}") from err
    if not isinstance(loaded, dict):
        raise MissingCredentials(f"Could not read credentials from {path}: expected a JSON object")
    return loaded


def load_credentials(*names: str, env: dict | None = None, path: Path | None = None) -> dict:
    """Return the requested credentials, or raise ``MissingCredentials``.

    The environment takes precedence over the file so a one-off run can
    override a stored value without editing it.
    """
    env = os.environ if env is None else env
    path = CREDENTIALS_FILE if path is None else path

    from_file = _from_file(path)

    values: dict[str, str] = {}
    missing: list[str] = []
    for name in names:
        raw = env.get(env_var_for(name)) or from_file.get(name) or ""
        value = str(raw).strip()
        if value:
            values[name] = value
        else:
            missing.append(name)

    if missing:
        wanted = ", ".join(env_var_for(name) for name in missing)
        raise MissingCredentials(
            f"Missing credentials: {wanted}.\n"
            f"Set them in the environment, or put them in {path} as a JSON object "
            f"keyed by lower-case name, for example:\n"
            f'  {{"{missing[0]}": "..."}}\n'
            f"That file is gitignored. Never put these values in a script.\n"
            f"See tools/credentials.json.example."
        )

    return values
