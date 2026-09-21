---
name: arc-journal
description: Write research notes, KPIs, attachments, and links to the ARC scientific journal with `arc journal`. Use when the user asks to journal, log, record, or note something for a project, when an experiment or measurement finishes, when a decision or dead end is worth keeping, or when the user asks what the journal says about a topic.
---

# ARC journal

The journal is an ARC provider. It stores markdown pages in a git repository.
Every write carries the caller's ARC key as author.

## Setup

Set two environment variables before every call:

```bash
export ARC_KEY=<writer key name>
export ARC_RELAY=127.0.0.1:7411
```

`ARC_KEY` is the name of a local key that writes. `arc keys list` lists the
names. Set it to the key that the project ACL allows. `ARC_RELAY` is the relay
that the journal provider listens on. If `ARC_RELAY` is not set, add
`--relay 127.0.0.1:7411` to the command line.

Test the connection:

```bash
arc journal ls
```

- If the call fails with `client: the peer did not answer` after 30 seconds,
  the provider is not running. See "Start the provider" below.
- If `arc` reports `no public key is pinned for 127.0.0.1:7411`, run
  `arc join 127.0.0.1:7411` once.
- If `arc` reports `unknown command "journal"`, install the tool:
  `arc install <journal provider public key> --yes`.
- If `arc` reports that the provider `serves another version now`, install
  the tool again with the same command.

## Address model

Every page has the address `<project>/<notebook>/<page>`.

- `project`: one research effort or codebase. Example: `arc`.
- `notebook`: one topic inside the project. Example: `journal`, `relay`.
- `page`: one document. Start the name with the date. Example:
  `2026-09-11-v1-status`.

Each segment must match `[a-z0-9][a-z0-9-_.]*`.

Run `arc journal ls` before you create a notebook. Reuse an existing
notebook when the topic fits.

## Commands

Read first:

```bash
arc journal ls                          # projects
arc journal ls arc                      # notebooks in a project
arc journal ls arc/journal              # pages in a notebook
arc journal read arc/journal/2026-09-11-v1-status
arc journal read arc/journal/2026-09-11-v1-status --lines 1:40
arc journal search "tool update" --project arc
arc journal history arc/journal/2026-09-11-v1-status
```

`read` returns `rev: <sha>` on the first line. Keep the rev if you plan to
edit the page.

Create or replace a page. The body comes from stdin:

```bash
arc journal write arc/journal/2026-09-11-v1-status --title "Journal provider v1 status" --tags "journal,status" <<'MD'
# Journal provider v1 status

Body in markdown.
MD
```

Add a note to the end of a page. This never conflicts:

```bash
arc journal append arc/journal/2026-09-11-v1-status "Tried lr=3e-4. Worse."
```

Change one string in a page. `--if-rev` is required:

```bash
arc journal edit <project>/<notebook>/<page> --if-rev <rev from read> --find "old text" --replace "new text"
```

Record a KPI. KPIs live outside the page and never rewrite history:

```bash
arc journal kpi set arc/journal tests_passing 23 --ref 80b5777 --note "go test"
arc journal kpi log arc/journal tests_passing
arc journal kpi latest arc/journal
```

If a page contains the line `<!-- kpi: tests_passing -->`, `read` shows the
latest value there.

Attach a file, or link to one that stays where it is:

```bash
arc journal attach arc/journal/2026-09-11-v1-status --name plot.png --base64 "$(base64 < plot.png)"
arc journal fetch <sha256>
arc journal link arc/journal/2026-09-11-v1-status file:///Users/zim/runs/lr-sweep --name run-dir
```

Attachments are capped at 16 MiB and are not backed up. Prefer `link` for
large or local files.

## Rules for agents

- Before you write, `ls` and `search` the project. Do not create a duplicate
  page for a topic that has one.
- Use `write` for a new page or a full rewrite. Use `append` for a note on an
  existing page. Use `edit` with `--if-rev` for a small correction.
- If `write` or `edit` fails with `conflict`, `read` the page again, then
  retry with the new rev.
- Write dense notes: what was tried, what was measured, what it means, and
  what is next. Name commits, files, and symbols.
- Record every measurement as a KPI. Put the commit SHA in `--ref`.
- Do not journal raw diffs or formatting-only edits.
- Never delete a page. If a page is obsolete, `write` an empty body with
  `--tags deleted`.

## Errors

Errors are one word, then optional detail: `forbidden`, `conflict`,
`not_found`, `too_large`, `invalid_address`, `invalid_number`,
`unknown_command`, `search_unavailable`. `arc` prints them after
`client: the peer answered provider_error:`.

- `forbidden`: the key is not on the project ACL. The project owner adds it
  with `arc journal acl <project> add <hex pubkey>`.
- `search_unavailable`: `qmd` is not on `PATH` on the provider host.

## Start the provider

Run these from the ARC repo root when `arc journal ls` fails with
`client: the peer did not answer`. If `bin/` is empty, run `mise run build`
first. The provider key is the key that owns the journal projects. The line
`<key name> serves on 127.0.0.1:7411` in `~/.arc/journal/serve.log` names the
key from the last run.

Start the relay:

```bash
nohup env ARC_KEY=<provider key name> bin/arc-relay --address 127.0.0.1:7411 > ~/.arc/journal/relay.log 2>&1 &
```

Pin the key of the relay. If the pin is already there, this changes nothing:

```bash
ARC_KEY=<provider key name> bin/arc join 127.0.0.1:7411
```

Start the provider:

```bash
nohup env ARC_KEY=<provider key name> JOURNAL_ROOT=$HOME/.arc/journal bin/arc serve cmd/journal-provider --relay 127.0.0.1:7411 > ~/.arc/journal/serve.log 2>&1 &
```

Do not run other `arc` commands as the provider key while the provider
runs. The relay keeps one connection for each key, so it drops the provider.

The first `serve` builds the binary. Wait for `arc journal ls` to answer.
The data lives in `~/.arc/journal/repo`. The spec is in
`docs/journal/SPEC.md` in the ARC repo.
