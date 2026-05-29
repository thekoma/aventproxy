# AGENTS.md

Operational guide for AI agents (Claude, Codex, Cursor, …) and human contributors working on this repo. For codebase reference, architecture, and commands see `CLAUDE.md` and `WHITEPAPER.md`; this file covers process and responsibilities.

## Release notes

Every release must ship with **user-facing release notes** — what changed, what users have to do (if anything), and what's bundled. The auto-generated "What's Changed" PR list produced by the release workflow is not enough on its own: it is a developer changelog, not a release note.

### Workflow

After the release workflow finishes (tag published, ghcr images built, `gh release view <tag>` works), the agent that drove the release is responsible for:

1. Drafting comprehensive notes for the freshly cut tag.
2. Writing them to a temp file (`/tmp/release-notes-<tag>.md`) and applying via:

   ```bash
   gh release edit <tag> --notes-file /tmp/release-notes-<tag>.md
   ```

3. Verifying the result with `gh release view <tag>`.

### Required sections

Lead with what users care about, then internals. Mandatory sections, in order:

- **Highlights** — one bullet per user-visible change. Mention the issue/PR number. Explain what the user gets, not what we did internally.
- **Documentation** — README/architecture-diagram/automation-example updates that are not bundled inside another highlight.
- **Internal / dependency updates** — dep bumps, refactors, log-format tweaks, anything maintainers care about but users don't.
- **Upgrade notes** — per user cohort: who has to do nothing, who has to re-add the integration, who has to restart, who has to wait for a backfill. Be specific about the cohorts (e.g. "SCD951 users on 2026.5.0", "users who added a monitor after initial setup").
- **Acknowledgements** — issue reporters, forum users who tested, anyone whose name should appear. Tag GitHub usernames with `@`; for forum users use the literal username without `@`.
- **Bundled tags** — applies when this release rolls up other releases (see the 24-hour rule below). One bullet per tag with a single-line summary.
- **Full Changelog** link to the `compare/<previous-stable>...<this-tag>` URL.

### The 24-hour roll-up rule

If a tag is being cut **within 24 hours of one or more earlier tags on the same trunk**, the new tag's notes must be a **superset** that absorbs the previous tag's notes:

- Treat the cluster of tags as one logical release line.
- The `Full Changelog` link compares against the **last stable that was outside the 24-hour window** (e.g. for `2026.5.2` cut 5h after `2026.5.0`, compare against the previous-day `2026.4.8`, not against `2026.5.1`).
- The `Bundled tags` section lists every tag in the cluster from oldest to newest, each with a one-line summary so a user reading any tag's notes sees the full picture.
- Do not silently leave the earlier tag's notes hanging — the assumption is the user installs the latest stable, not the cluster middle.
- If the cluster grows further (e.g. a 4th release tomorrow within 24h of the 3rd), repeat: edit the new tag's notes to roll up the whole 24-h window, and leave the earlier tags' notes as they were.

### Style

- Use Markdown headings exactly as in the structure above.
- One sentence per bullet when possible; second sentence only if it adds operational detail.
- No marketing language ("blazing fast", "exciting", etc.). Match the project's neutral technical tone.
- Reference real anchors users can grep: entity names (`binary_sensor.<camera>_sound_detected`), file paths, port numbers, version strings.
- Always link issues and PRs by number (`#33`, `#41`). Always link forum threads by full URL.

## Other agent expectations

- **Codebase reference is `CLAUDE.md`.** It documents the three components (HA integration / Go bridge / HA add-on), the API signing flow, DPS codes, and how to run the test suites locally. Read it before edits.
- **Reverse-engineering provenance is `WHITEPAPER.md`.** Touch sparingly — it's the public-facing methodology.
- **User-facing config is zero manual steps.** The add-on is driven by the integration, the integration by the config flow. Don't add YAML options, env vars, or manual ID propagation. New features must propagate end-to-end through `config_flow → entry.data → __init__.py → bridge config JSON → addon run.sh → bridge binary`.
- **Tests live in two languages.** Python (`tests/test_philips_avent/`) and Go (`avent-webrtc-bridge/.../*_test.go`). Cross-language contracts (such as `sanitize_rtsp_path` / `SanitizeRTSPPath`) get tested on both sides; the shared helper lives in `custom_components/philips_avent/payload.py` and `avent-webrtc-bridge/pkg/storage/path.go`.
- **CI mirror locally.** No local Go installed; use `golang:1.26-bookworm` container for `go test`/`go build`. Python tests/ruff are normal `pytest`/`ruff` invocations. The full GitHub workflow can also be replayed via `act` (see the `feedback-local-ci` and `reference-local-tools` memories if working from a Claude session that has access to them).
