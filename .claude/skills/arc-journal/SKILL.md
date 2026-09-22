---
name: arc-journal
description: Write research notes and KPIs to the ARC journal with `arc journal`. Use when the user asks to journal, log, record, or note something for a project, when an experiment or measurement finishes, when a decision or dead end is worth keeping, or when the user asks what the journal says about a topic.
---

# ARC journal

The journal is a capability of `arc`, described by `manifests/journal.json`.
A page is a NIP-37 draft, sealed to your own key. Only your key reads it. A
relay stores it, and cannot read it. Every machine that holds the same key
reads the same pages. See `docs/interface/SPEC.md`, section 7.2.

The journal of the older stack, the journal provider, is gone. Its pages stay in the git repository at `~/.arc/journal/repo`. The new
journal does not read them.

## Setup

Do these steps one time for each machine. Check each step before the next.

1. Make or add the identity. The first identity is the default.

   ```bash
   arc keys list
   arc keys gen                  # a new identity
   arc keys add < <key file>     # the key of another machine of this citizen
   ```

   `--key <name>` or `ARC_KEY` picks another identity for one command.

2. Add a relay:

   ```bash
   arc relay add wss://arc-nostr-gezim.fly.dev
   ```

3. Install the journal from its author. If no author announced it, announce
   it yourself from the root of the ARC repository, and install it from your
   own key:

   ```bash
   arc announce manifests/journal.json
   arc install "$(arc whoami | sed -n 2p)" journal --yes
   ```

4. Test it:

   ```bash
   arc journal ls
   ```

If `arc` reports `unknown command "journal"`, do step 3. If `arc` reports that
the author changed what the journal can do, install it again with the same
command.

## Address model

Every page has the address `<project>/<notebook>/<page>`.

- `project`: one research effort or codebase. Example: `arc`.
- `notebook`: one topic inside the project. Example: `journal`, `relay`.
- `page`: one document. Start the name with the date. Example:
  `2026-09-22-v1-status`.

Each segment must match `[a-z0-9][a-z0-9_.-]*`. Lower case only.

Run `arc journal ls <project>` before you create a notebook. Use an existing
notebook when the topic fits.

## Commands

Read first:

```bash
arc journal ls                          # every page: address, title, date
arc journal ls arc                      # the pages whose address starts with arc
arc journal ls arc/journal              # the pages of one notebook
arc journal read arc/journal/2026-09-22-v1-status
arc journal read arc/journal/2026-09-22-v1-status --lines 1:40
arc journal search tool update          # address, score, title
arc journal history arc/journal/2026-09-22-v1-status
```

`read` of a page that does not exist prints nothing, and exits 0.

Make or replace a page. The body comes from standard input:

```bash
arc journal write arc/journal/2026-09-22-v1-status --title "Journal v1 status" <<'MD'
# Journal v1 status

Body in markdown.
MD
```

Add text to the end of a page. Each word after the address is part of the
text:

```bash
arc journal append arc/journal/2026-09-22-v1-status "Tried lr=3e-4. Worse."
```

Show each text as it is appended, on this machine or another. Stop it with
Ctrl-C:

```bash
arc journal tail arc/journal/2026-09-22-v1-status
```

Record a KPI. A KPI lives outside the pages. The notebook argument is any
text. Use the address of the notebook:

```bash
arc journal kpi set arc/journal tests_passing 23 --note "go test at 80b5777"
arc journal kpi log arc/journal tests_passing     # every value
arc journal kpi latest arc/journal                # the last value of each key
```

Delete a page and its history. Do this only when the user asks:

```bash
arc journal delete arc/journal/2026-09-22-v1-status
```

`arc help journal` lists the commands.

## What the new journal does not have

The older journal had these features. The new journal does not have them. Do
not use them, and do not invent a replacement.

- `edit` with `--find`, `--replace` and `--if-rev`. Use `read`, then `write`
  the whole page.
- `--tags` on `write`.
- A `rev` line in `read`, and `conflict` errors.
- Project ACLs and `acl add`. A page is private to your key.
- `search --project`. Search takes words only, and searches every page.
- `kpi set --ref`. Put the commit in `--note`.
- A KPI line in a page, `<!-- kpi: ... -->`.
- `attach`, `fetch` and `link`. Write the path or the URL in the page text.

## Rules for agents

- Before you write, `ls` and `search`. Do not make a second page for a topic
  that has one.
- Use `write` for a new page or a full rewrite. Use `append` for a note on a
  page that exists.
- Write dense notes: what you tried, what you measured, what it means, and
  what is next. Name commits, files and symbols.
- Record each measurement as a KPI. Put the commit SHA in `--note`.
- Do not journal raw diffs or edits that change only the format.
- Do not delete a page unless the user asks. For an obsolete page, `append`
  a line that says so.

## Without a relay

If no relay answers, `write` and `append` keep the page in the store of this
machine, and say `it waits in the store; arc sync sends it`. `read` then
shows what this machine holds. When a relay answers again, run:

```bash
arc sync
```

`arc sync --dir <path>` carries the pages on a USB stick or a shared folder
instead. Run it on each machine.

## Errors

- `unknown command "journal"`: install the journal, see "Setup".
- `address: "..." does not match ...`: an address segment has upper case or
  another character that is not allowed.
- `no relay answered`: the relay is down or the URL is wrong. See "Without a
  relay".
