---
name: arc-journal
description: Keep notes and KPIs in the ARC journal with `arc journal`. Use when the user asks to journal, log, record, or note something, when a release, a proof, or a measurement finishes, when a decision, a root cause, or a dead end is worth keeping, or when the user asks what the journal says about a topic. Works in any project, not only the arc repository.
---

# ARC journal

The journal is a private notebook of one ARC identity. `arc` itself runs it.
No provider takes part.

- Each page and each KPI is sealed to the key of the identity. Only that key
  reads it. The relay stores it, and cannot read it.
- Each machine that holds the same key reads the same pages.
- A machine with a different key has a different journal. It cannot read the
  pages of another key.

The journal works from any directory. You do not need the arc repository.

## Check the setup

Run these commands before the first write in a session:

```bash
arc whoami          # the identity: a petname, then the public key
arc journal ls      # the pages of this identity
```

If `arc whoami` fails, no identity exists. Ask the user before you make one.
Do not make a new identity by yourself, because a new key starts an empty
journal.

If `arc` reports `unknown command "journal"`, install the journal. The owner
of the journal announced it as `tidal-picard-c614cd5b`:

```bash
arc relay ls        # must list wss://arc-nostr-gezim.fly.dev
arc install e04bdd198cd9741309e30e6c8bf0c25fa7a3587565929124e26a601c5870948b journal --yes
```

If `arc relay ls` does not list that relay, add it:

```bash
arc relay add wss://arc-nostr-gezim.fly.dev
```

If `arc` reports that the author changed what the journal can do, run the
same `arc install` command again.

## Addresses

Each page has the address `<project>/<notebook>/<page>`.

- `project`: the codebase or effort. Use the name of the repository, for
  example `arc`, `promoset` or `tezca`.
- `notebook`: one topic in the project, for example `status`, `relay` or
  `perf`. Use `status` for releases and for the state of the project.
- `page`: one document. Start the name with the date, for example
  `2026-09-23-v0-16-0-release`.

Each segment must match `[a-z0-9][a-z0-9_.-]*`. Use lower case only. Use `-`
in place of a space.

Before you make a notebook, run `arc journal ls <project>`. If a notebook fits
the topic, use it.

## Read

```bash
arc journal ls                                   # each page: address, title, date
arc journal ls arc/status                        # the pages whose address starts with arc/status
arc journal read arc/status/2026-09-23-v0-16-0-release
arc journal read arc/status/2026-09-23-v0-16-0-release --lines 1:20
arc journal search relay reconnect               # address, score, title
arc journal history arc/status/2026-09-23-v0-16-0-release
arc journal tail arc/status/2026-09-23-v0-16-0-release   # each append as it comes; Ctrl-C stops it
```

- `search` finds words in the title and the text of each page. It searches
  every page of every project.
- If a page does not exist, `read` prints nothing and exits 0.

## Write

`write` makes a page, or replaces the full page. The body comes from standard
input. Markdown is the format:

```bash
arc journal write arc/relay/2026-09-23-port-race --title "Port race in the relay test" <<'MD'
# Port race in the relay test

- Cause: the test released a port, and another test process took it.
- Fix: the relay keeps its port while it is down. PR #126.
MD
```

`append` adds one line to the end of a page. Each word after the address is
part of the text. If the text starts with `-`, put `--` before it:

```bash
arc journal append arc/relay/2026-09-23-port-race "20 of 20 runs pass."
arc journal append arc/relay/2026-09-23-port-race -- "- Open: the race in slicestore.QueryEvents."
```

To change text in the middle of a page, `read` the page, change the text, and
`write` the full page.

## KPIs

A KPI is one measured value with a key. It is not in a page. The first
argument names the notebook. Use `<project>/<notebook>`:

```bash
arc journal kpi set arc/perf call_round_trip_ms 18.4 --note "mise run compose at a91baf4"
arc journal kpi latest arc/perf                      # the last value of each key
arc journal kpi log arc/perf call_round_trip_ms      # each value of one key
```

Put the commit SHA in `--note`. No command deletes a KPI. Do not record a
test value.

## When to write

Write a page when one of these finishes:

- A release. Write the version, the tag commit, and what it adds or breaks.
- A proof or a measurement. Record each number as a KPI too.
- A decision. Write what you chose, what you rejected, and why.
- A root cause of a bug. Write the cause, the fix, and the PR.
- A dead end. Write what you tried, and why it failed.

Do not write a page for each small step. Do not journal diffs, logs, or
changes of format only.

## Rules

- Before you write, run `ls` and `search`. If a page covers the topic,
  `append` to it. Do not make a second page.
- Keep notes dense. Name the commits, the PRs, the files and the symbols.
- Write facts that you checked. If a claim is not checked, say so in the
  text.
- Do not write a secret, a key, or a token into a page.
- Delete a page only when the user asks. If a page is out of date, `append`
  a line that says so. `delete` removes the page and its history.

## Limits

- A page has no tags. Put words in the title or the text, so `search` finds
  them.
- A page holds no files. Write the path or the URL in the text.
- `search` has no filter for a project. Use `ls <project>` to limit the list.
- Two writes of one page at the same time do not merge. The last `write`
  replaces the page.

## Without a relay

If no relay answers, `write` and `append` keep the page on this machine and
print `it waits in the store; arc sync sends it`. When a relay answers again,
run:

```bash
arc sync
```

`arc sync` prints many lines that start with `[nl][info]`. They are log noise
from the Nostr library. Read only the last lines, which count what was sent
and received.

`arc sync --dir <path>` syncs through a USB stick or a shared folder. Run it
on each machine.

## Errors

- `unknown command "journal"`: install the journal. See "Check the setup".
- `address: "..." does not match ...`: a segment has upper case or a character
  that is not allowed.
- `unknown flag`: the text of `append` starts with `-`. Put `--` before it.
- `no relay answered`: the relay is down or its URL is wrong. See "Without a
  relay".
