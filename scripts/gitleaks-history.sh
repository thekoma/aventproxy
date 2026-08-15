#!/usr/bin/env bash
# Scan the entire git history for secrets, not just the working tree.
#
# Uses the gitleaks container so it works without a local install; falls back
# to a gitleaks on PATH if one is there. Pinned to match
# .pre-commit-config.yaml and the CI job.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if command -v gitleaks >/dev/null 2>&1; then
  exec gitleaks detect --source=. --config=.gitleaks.toml --redact --no-banner "$@"
fi

if ! docker info >/dev/null 2>&1; then
  echo "Neither a gitleaks binary nor a running Docker daemon was found." >&2
  echo "Install gitleaks, or start Docker, then re-run." >&2
  exit 2
fi

exec docker run --rm -v "$PWD:/repo" -w /repo zricethezav/gitleaks:v8.30.0 \
  detect --source=. --config=.gitleaks.toml --redact --no-banner "$@"
