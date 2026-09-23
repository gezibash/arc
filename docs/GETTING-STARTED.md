# Getting started with ARC

ARC lets people and programs exchange private messages and use services offered
by other participants. A **capability** is something a provider offers, such as
querying a database. Your **identity** is a cryptographic key pair: the public
key is your address, and the private key proves that you control it.

This guide takes you from installation to a first message or capability call.
Commands describe the current Go CLI. Start with `arc version` if ARC is already
installed; use `arc --help` and subcommand help if your version differs. For an
older installation, read the [upgrade notes](../README.md#upgrade-from-v0100)
before moving files or generating a replacement identity.

## 1. Install or check ARC

The release installer supports Linux x86_64 and aarch64 with glibc 2.36 or newer,
and macOS on Apple silicon. For other systems, see
[building from source](../README.md#development).

If ARC is already installed:

```sh
arc version
```

Otherwise, the following downloads and runs ARC's installation script. It
installs for your user without root access, checks the release archive against
its published checksums, and places the command in `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/gezibash/arc/main/install.sh | sh
arc version
```

If your terminal says `arc: command not found`, try:

```sh
export PATH="$HOME/.local/bin:$PATH"
arc version
```

That PATH change lasts for this terminal session. Add it to your shell's startup
configuration if needed. The [README](../README.md#install) also describes manual
installation and choosing a specific release.

**Checkpoint:** ARC prints its version.

## 2. Choose your identity

Check for an existing identity first:

```sh
arc keys list
arc whoami
```

If you already have the identity you want, keep it. To select one listed by
`arc keys list`, replace `IDENTITY_NAME` with its name:

```sh
arc keys use IDENTITY_NAME
arc whoami
```

If this is a fresh setup with no identity, create one:

```sh
arc keys gen --encrypt
arc whoami
```

Enter the passphrase in the terminal when prompted. Keep it private. The public
key printed by `whoami` is safe to share as your address. Never share the private
key or paste it into an assistant conversation.

By default, each identity lives under `~/.config/arc/citizens/`. Back up the
identity's key file to a private, secure location, and keep the passphrase
available separately. A copy of the identity directory also preserves local
state; stop ARC processes before copying it. If you use `--home` or `ARC_HOME`,
your files live there instead. A remote signer needs its own backup procedure.
Losing the private key and all its backups means losing control of that identity.

The first identity becomes the default. `--key IDENTITY_NAME` selects an identity
for one command without changing the default. `ARC_KEY` can also select one.

**Checkpoint:** `arc whoami` shows the identity you intended to use.

## 3. Connect to a relay

A relay is a server that carries events between participants. Private message
content is encrypted, but relays can still observe delivery metadata. Adding a
relay can publish your relay lists so others can find where to reach you.

First check your existing list:

```sh
arc relay ls
```

Use a relay you have chosen. The README gives this example; its availability is
not guaranteed:

```sh
arc relay add wss://arc-nostr-gezim.fly.dev
arc sync
```

Keep an existing working relay rather than adding another unnecessarily. An
optional indexer helps participants find each other's relay lists; it is not
needed for the initial shared-relay example. See the
[README quick start](../README.md#quick-start).

**Checkpoint:** the relay is listed and sync completes without a connection
error. An empty sync is normal for a new identity.

## 4. Send a first message

Ask the recipient for their public key and arrange a shared relay for this first
test. Replace `RECIPIENT_PUBLIC_KEY` below with their key:

```sh
arc message send RECIPIENT_PUBLIC_KEY "Hello from ARC"
arc message outbox
```

On the recipient's machine, using their own identity:

```sh
arc sync
arc message inbox
```

On your machine, sync again to collect any acknowledgement:

```sh
arc sync
arc message outbox
```

A queued message or successful send command does not by itself prove receipt.
The message can wait in the outbox until the recipient acknowledges it.

**Checkpoint:** the recipient sees your message. If no recipient is available,
you can finish local setup now and verify delivery later.

## 5. Try a capability

A provider is another ARC participant offering a capability. Discovery searches
announcements available through your configured delivery paths:

```sh
arc discover
```

Choose a provider you intend to trust. Discovery shows its public key and the
capability identifier. Replace both placeholders:

```sh
arc install PROVIDER_PUBLIC_KEY CAPABILITY_ID
```

Read the description and trust prompt before accepting. Installing records your
consent to use that capability; it does not start the provider's service on
your machine. The provider must be available to answer a live call.

For a capability that adds commands, the installer prints the installed name:

```sh
arc help INSTALLED_NAME
```

Follow that help for your first call. For example, if you deliberately installed
an exec capability under the name `exec`, this asks the provider machine to
report its operating-system information:

```sh
arc exec run uname -a
```

It runs on the provider's machine. Only use this example with a provider and
capability you chose for that purpose. Some capabilities are called with
`arc call` instead; follow the installer's output.

**Checkpoint:** you receive the expected reply from the selected provider.
No discovery results means no matching announcements were found; it does not
mean your identity is broken.

## Offline use

ARC can also exchange events through a directory, such as a USB drive or shared
folder:

```sh
arc sync --dir /path/to/exchange
```

Run it on each participating machine against the same carried directory. Each
machine exports and imports events during its sync; replies need a return trip.
This is not an instant connection. Private events stay encrypted in transit.

Bluetooth is planned in the baseline covered by this guide. A compact message
encoder alone does not enable the radio. Check the installed release's status
before expecting device discovery or Bluetooth calls. A capability that depends
on an internet service will still need that service even with offline delivery.

## If something does not work

| Symptom | Next step |
| --- | --- |
| `arc` is not found | Check installation and PATH in step 1. |
| No identity, or the wrong identity | Check `arc keys list`, `--home`, `ARC_HOME`, `--key` and `ARC_KEY` before generating a new key. |
| A key needs its passphrase | Run the command in a terminal and enter it privately. |
| No relay answers | Check the URL, connection and chosen relay's availability. Keep the existing identity. |
| No capabilities found | Confirm the provider has announced on a reachable relay, or sync its announcements through a directory. |
| A message stays in the outbox | Ask the recipient to sync, then sync your sender identity again. |
| An installed command fails | Read `arc help INSTALLED_NAME`; check provider availability and the reported error before retrying. |
| An old example names an unknown command | Use your installed command help and the matching release documentation. |

For assistant-guided setup, this repository includes the
[arc-onboarding skill](../.claude/skills/arc-onboarding/SKILL.md). In an assistant
that discovers this repository's skills, ask: “Use arc-onboarding to help me set
up ARC.” Otherwise, point the assistant to that file and this guide. Keep the
repository layout when copying the skill, or provide the guide separately.
