# Reporting an issue

If something isn't working — wrong sensor value, missing motion alert, broken video, anything — the integration can give us a snapshot of its internal state that's far more useful than a screenshot or a description. **You don't need to be a developer to collect it.** The whole flow is three clicks inside the Home Assistant UI.

This page is what to send along with the bug report.

---

## What we need

1. **A diagnostics dump** — the contents of every sensor and switch right now, including the raw values coming from your monitor.
2. **(Optional, but very helpful) a short debug log** — captured while you trigger the broken behavior (e.g. make a noise to test sound detection).
3. **One sentence** describing what happened vs. what you expected.

That's it. Both files are JSON / text that you can attach to a GitHub issue. The integration **automatically removes** your password, session token, email address, and device keys before you download anything.

---

## Step 1 — download the diagnostics dump

1. Open Home Assistant.
2. Go to **Settings → Devices & Services**.
3. Find **Philips Avent Baby Monitor** in the list and click on it.
4. In the top-right corner of the integration card, click the three dots (**⋮**) menu.
5. Click **Download diagnostics**.

A file called `config_entry-philips_avent-…json` is saved to your computer. **Don't open it to clean it up** — the redaction is already done. Just keep it ready to attach to the issue.

If you have multiple Philips Avent monitors on one account, the file already covers all of them.

---

## Step 2 — capture a short debug log (only if your bug is about an event that happens at a specific moment)

This step is needed when the bug is something like:
- "motion detection should fire when someone walks in front of the camera, but the sensor never turns on"
- "the sound alert switch is on, but `binary_sensor.…_sound_detected` doesn't react when the baby cries"
- "the night light command takes 30 seconds to take effect"

If your bug is just a constant wrong value (e.g. the RSSI sensor always shows -75 dBm), skip this step — the diagnostics dump already has it.

### Enable debug logging

1. **Settings → Devices & Services → Philips Avent Baby Monitor**.
2. Click the three dots (**⋮**) menu in the top-right of the integration card.
3. Click **Enable debug logging**.

Home Assistant now records everything the integration does, in detail.

### Trigger the broken behavior

Do the thing that you expect to set off the event. For example:
- For **motion detection**: walk slowly in front of the camera for ~10 seconds.
- For **sound detection**: clap your hands twice loudly near the monitor, or play a short loud noise (a few seconds) from a phone next to it.
- For **night light response time**: turn the night light on and off twice from Home Assistant. Note roughly how many seconds before the device reacts.

Do this **two or three times** so we have a few independent samples in the log.

### Disable + download

1. Same three-dots menu (**⋮**) on the integration.
2. Click **Disable debug logging**.

Home Assistant automatically prompts you to download a `home-assistant_philips_avent_…log` file. Save it.

---

## Step 3 — attach the files to a GitHub issue

1. Go to [https://github.com/thekoma/aventproxy/issues](https://github.com/thekoma/aventproxy/issues).
2. Either comment on the existing issue you're contributing to (e.g. [#40](https://github.com/thekoma/aventproxy/issues/40) for SCD921 problems) **or** open a new one with the **Bug report** template.
3. Drag-and-drop the diagnostics JSON file (and the log file if you captured one) into the comment box. GitHub uploads them as attachments and shows a link.
4. Add a one-sentence description: "I expected X, I got Y, and here's the dump."

That's everything. The dump contains the actual sensor values and the raw codes the monitor reports, which is what we use to figure out why your model behaves differently from the development device (SCD973).

---

## Privacy — what's in the files (and what's not)

The integration redacts these fields automatically before any download:

| Redacted | Kept |
|---|---|
| Email address | Device model and firmware version |
| Password / Tuya session token | Device serial / Tuya ID (needed for triage) |
| Tuya `ecode` / `partner_identity` / `local_key` | Sensor readings and DPS payloads |
| HA user id | Wi-Fi RSSI value |

If you want to double-check, you can open the JSON in any text editor before attaching — the redacted fields literally say `"**REDACTED**"`. Nothing else is sent anywhere; the file only goes wherever you upload it.

---

## What if I'm still uncomfortable?

That's fine. Open an issue with just the **one-sentence description** and a screenshot. We'll ask follow-up questions and the dump can come later. The integration is run by a parent in their spare time — there is no automated data collection and we do not have access to anything you don't explicitly share.
