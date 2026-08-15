# Security

## Reporting a vulnerability

Please don't open a public issue for anything that could expose someone's
account or camera. Use GitHub's private reporting instead:
[**Report a vulnerability**](https://github.com/thekoma/aventproxy/security/advisories/new).
That opens a draft advisory only the maintainer can see.

If you'd rather not use GitHub, email `the@k8s.one`.

Tell me what you found and how you found it. A rough description beats no
report — you don't need a proof of concept, and you don't need to propose a
fix. I'll confirm receipt, and I'll credit you when the fix ships unless you
ask me not to.

## What this project touches

The integration holds a Tuya session for your Philips Avent account and, with
it, the ability to watch and control your baby monitor. Things worth reporting:

- anything that writes credentials somewhere they can be read — a log, a file
  with loose permissions, a diagnostics dump, a backup
- anything that lets a device or account other than yours reach your stream
- anything in this repository's history or working tree that looks like a real
  credential rather than a placeholder

Two known properties that are **not** vulnerabilities, because they're the
documented design:

- The RTSP stream has no authentication and the add-on runs with
  `host_network: true`, so anything on your LAN can watch it. Isolate the
  monitor on its own network segment if that matters to you.
- `const.py` carries identifiers extracted from the Philips Avent APK — the
  package name, certificate hash, app key and signing key. They're identical
  for every install of that app, they identify the app rather than any user,
  and the project cannot talk to Tuya without them. See `WHITEPAPER.md`.

## Never commit credentials

A real account's `sid`, `ecode` and camera `localKey` sat in four scripts under
`tools/` for three months before a reader found them. Rotating a leaked
credential is disruptive, and rewriting published history is worse. So:

**Development scripts read credentials from the environment, or from
`tools/credentials.json`, which is gitignored.** Copy
`tools/credentials.json.example` and fill it in. See `tools/_credentials.py`.
No script may hold a working value, not even temporarily, not even in a branch
you intend to squash.

Two gates enforce this:

```bash
pip install pre-commit && pre-commit install     # blocks the commit locally
```

CI runs the same [gitleaks](https://github.com/gitleaks/gitleaks) scan over the
full history on every push. Rules live in `.gitleaks.toml`, and they include
project-specific patterns because the stock rules miss the ones that matter
here: `ECODE` and `LOCAL_KEY` are short and low-entropy, and the generic
high-entropy detector walks straight past both.

To scan the whole history yourself:

```bash
./scripts/gitleaks-history.sh
```

## If you do leak one

In this order, because each step depends on the previous one:

1. **Change the Philips Avent account password.** This invalidates the `sid`,
   which is the credential that authenticates API calls on its own.
2. **Re-pair the camera.** This rotates the `localKey`, which grants direct LAN
   control and is not affected by the password change.
3. **Remove the value from the code**, and make the script read it from the
   environment instead.
4. **Rewrite the history** with `git filter-repo --replace-text`, then force
   push. Do this last: it does not help anyone who already cloned, which is why
   rotation comes first.
