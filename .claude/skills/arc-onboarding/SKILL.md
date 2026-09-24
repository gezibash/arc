---
name: arc-onboarding
description: Help a user install ARC, select or create an identity, configure a relay, and complete a first message or capability call. Use for ARC setup, getting started, or troubleshooting first use.
---

# ARC onboarding

Help the user reach one working ARC workflow, explaining each step in plain
language. ARC uses signed Nostr events for messages; it adds capability
discovery, provider calls and delivery. Nostr events do not require internet,
but a WebSocket relay does.

Read [Getting started](../../../docs/GETTING-STARTED.md) for the walkthrough,
expected results and troubleshooting. This link assumes the repository layout.
If this skill was copied elsewhere, locate the guide in the user's ARC checkout
or read `https://github.com/gezibash/arc/blob/main/docs/GETTING-STARTED.md`.
Do not guess commands when the guide is unavailable: inspect `arc --help` and
relevant subcommand help. Installed-version help takes precedence over examples
for a newer version.

## Choose the path from the user's situation

- Check the operating system, architecture, `arc version`, and the relevant
  command help. If ARC is missing, follow the guide's installation section.
  A request to write or explain setup instructions is not a request to install.
- Inspect `arc keys list`, `arc whoami` and `arc relay ls` when available.
  Preserve the selected `--home`, `ARC_HOME`, `--key` and `ARC_KEY`; inspect only
  these nonsecret settings when needed, never dump the whole environment.
- Reuse the intended identity. A failing `whoami` can mean a wrong selector,
  not missing keys. Create an identity for a requested fresh setup only after
  checking existing identities; resolve ambiguity with the user. Never replace
  a key or silently migrate an older home.
- Use the user's relay choice. The guide's public URL is an example, not a
  requirement or a guarantee of availability. Adding a relay may publish relay
  lists; explain that briefly before configuring it. Do not add an indexer or
  start a relay server merely to finish basic setup.
- Continue with the requested first message or capability. If no preference
  was given, ask which outcome they want while completing independent checks.

## First use

For a message, obtain the recipient and text from the user before sending.
Explain that an outbox entry is queued, not proof of receipt. Have the recipient
sync and check their inbox; distinguish local setup, relay acceptance and
confirmed delivery in the result.

For a capability, discover offers, identify the intended provider and explain
what is being trusted. Do not choose an arbitrary provider or use `--yes`
without existing authorization to trust that provider and capability. Follow
the installed capability's help for a small requested call. Prefer a read-only
example; do not execute remote commands merely because discovery succeeded.
A missing provider does not require creating a different identity.

Keep private keys, passphrases and signer secrets out of tool output and chat.
Have the user enter passphrases through a private terminal prompt. Explain key
backup using the guide; do not open or display key files to verify the backup.

Bluetooth is planned in the baseline described by this guide. Check the actual
release before claiming radio support; compact encoding alone is not Bluetooth
connectivity. Directory sync is the existing offline path.

## Finish

Report the version, selected public identity, configured transport, and the
workflow actually verified. Name any missing recipient, provider or connection
that prevented completion, and give the next concrete step. Do not claim
successful delivery from an exit code alone. Do not turn onboarding into relay
hosting, provider deployment or key migration unless requested.
