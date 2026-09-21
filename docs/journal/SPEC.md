# Journal: a private notebook for agents on ARC

Status: replaced. The journal now runs on the delivery layer, without a
provider: see the `journal` package and docs/delivery/SPEC.md. This document
describes a provider design that was never built. A rewrite on the delivery
layer is pending.

## 1. Purpose

The journal is a private notebook. An agent writes research notes, links to
files, and records KPIs. Only the agent that wrote an entry can read it.

The provider stores entries and returns them. It cannot read them. The
client, which is `arc` on the owner's machine, does all work that needs the
content: rendering, listing, search, and KPI summaries.

The journal needs no other program. The provider and the client are Go code
in this module. Neither one runs Git, qmd, or a database server.

## 2. Scope

A journal belongs to one owner. The owner is one ARC identity. The journal of
an owner is the set of entries that the owner signed.

These are out of scope:

- Sharing a journal, or a part of it, with another identity.
- Public read access.
- Semantic search. Search is keyword search only.
- Storing file bytes. A page links to a file. It never holds the file.
- Hard deletion of entries from the provider.
- Recovery of a lost key.

## 3. Terms

| Term | Meaning |
| --- | --- |
| owner | The identity that writes and reads one journal. |
| client | The `arc` program of the owner. |
| provider | The citizen that stores entries for many owners. |
| entry | One signed, sealed envelope that the provider stores. |
| page revision | An entry of kind `page`. It holds one version of a page. |
| KPI record | An entry of kind `kpi`. It holds one measured value. |
| stream | All entries that have one stream ID. |
| head | The newest page revision of one page stream. |
| rev | The SHA-256 hash of one entry, as lower-case hex. |
| sequence | The position of an entry in the log of its owner. |
| cursor | The highest sequence that the client holds. |

## 4. What the provider can and cannot do

The design assumes that the provider can be hostile. The provider holds only
entries, and each entry is sealed and signed.

| The provider does this | Result | Defense |
| --- | --- | --- |
| Reads an entry | It sees ciphertext only. | The body is sealed to the owner. |
| Changes an entry | The signature fails on the client. | The owner signs every entry. |
| Adds an entry of its own | The signature fails on the client. | Only the owner's key signs. |
| Moves an entry to another page | The signature fails on the client. | The signature covers the stream ID. |
| Serves an old head as new | The client stops the sync. | A page revision must name the held head as parent. |
| Serves one entry twice | The client stops the sync. | The client refuses a rev that it holds. |
| Serves a different entry under a rev | The client stops the sync. | The hash of the entry must be its rev. |
| Hides an entry in the middle of the log | The client stops the sync. | Sequences must have no gap. |
| Hides an entry that this client wrote | The client stops the sync. | The log must reach each sequence that `put` returned. |
| Shows two machines different logs | The client stops the sync later. | See the note below. |
| Deletes all entries | The data is gone from the provider. | Each synced client holds a full copy. |

A provider can show two machines of the owner different logs. It can also
hide the newest entries of one machine from another. In both cases, each
machine sees a consistent history. The split becomes visible when one machine
syncs an entry that the other machine wrote after the split. That entry names
a parent that the first machine does not hold as its head.

The provider learns this metadata:

- The owner's public key.
- The kind of each entry: page revision or KPI record.
- The number of streams, and the number of entries in each stream.
- The size of each entry.
- The time at which each entry arrived.

The provider does not learn addresses, titles, tags, page text, KPI names,
KPI values, or links.

## 5. Keys

The client derives one key from the owner's seed:

```text
id_key = HKDF-SHA256(ikm = seed, salt = "", info = "arc-journal-v1 id", length = 32)
```

The client uses `id_key` to turn a name into a stream ID. The provider never
sees the name.

```text
page stream ID = hex(HMAC-SHA256(id_key, "page" || 0x00 || address))
KPI stream ID  = hex(HMAC-SHA256(id_key, "kpi"  || 0x00 || project "/" notebook))
```

Each machine that holds the seed derives the same IDs. The owner therefore
uses the same journal from each machine.

The client seals each body to the owner's own public key with the sealed box
of the `sealedbox` package. The client signs each entry with the owner's
Ed25519 key.

If the owner loses the seed, the journal cannot be read. Nothing recovers it.

## 6. The entry

An entry is one JSON object:

```json
{
  "version": 1,
  "kind": "page",
  "stream": "<stream ID, 64 hex>",
  "parent": "<rev of the previous head, or empty>",
  "author": "<owner public key, 64 hex>",
  "body": "<base64 of the sealed body>",
  "signature": "<base64 of the Ed25519 signature>"
}
```

Rules:

- `kind` is `page` or `kpi`.
- `parent` is empty for the first revision of a page. A KPI record has an
  empty `parent`.
- The signature covers the domain `ARC-JOURNAL-ENTRY-V1`, then one zero byte,
  then the canonical JSON of every field except `signature`.
- Canonical JSON is the encoding of `internal/canonical`: sorted keys, no
  spaces, and numbers with their digits kept.
- The rev of an entry is the SHA-256 hash of the canonical JSON of the whole
  entry, with `signature` included.
- The sealed box uses a new ephemeral key for each body. Two entries with the
  same text therefore have different revs.

## 7. Pages

### 7.1 Address

Every page has an address: `<project>/<notebook>/<page>`.

- `project`: a research effort. Example: `hrs`.
- `notebook`: a topic inside the project. Example: `ablations`.
- `page`: one document. Example: `2026-09-11-lr-sweep`.

Each segment must match `[a-z0-9][a-z0-9-_.]*`. The client refuses other
characters.

### 7.2 Body

The sealed body of a page revision is a Markdown file with YAML frontmatter:

```markdown
---
address: hrs/ablations/2026-09-11-lr-sweep
title: LR sweep on HRS
created: 2026-09-11T14:02:11Z
updated: 2026-09-11T15:40:00Z
tags: [ablation, lr]
refs:
  - github: owner/repo@df62851
links:
  - name: run-dir
    uri: file:///Users/zim/runs/lr-sweep
    host: 9f3a...c1      # public key of the machine that owns the path
---

Body in Markdown.
```

The client owns `address`, `created`, `updated`, and `links`.
The owner sets `title`, `tags`, and `refs`.

The client needs `address` to list pages. A stream ID does not reveal it.

### 7.3 Links

A link records where a file lives. The journal stores no file bytes. The bytes
stay where the link points, and the owner keeps them there.

- `uri` is any URI: `file://`, `https://`, `s3://`, `arc://`, or a `github:`
  reference.
- If the URI scheme is `file://`, the client sets `host` to the owner's public
  key. The path is valid on that host only.
- If the owner gives `--sha256`, the client stores it. The client does not
  verify it.
- The client never follows a link.
- A link is part of the sealed page, so the provider never sees it.
- If a file must stay private, the owner links to a private place. The journal
  does not protect the file itself.

### 7.4 Deletion

To delete a page, write a revision with an empty body and the tag `deleted`.
The client hides a page with that tag from `ls` and `search`. The provider
keeps every revision.

## 8. Revisions and concurrency

The provider keeps one head for each page stream. It accepts a page revision
only if `parent` is the current head.

- If `parent` is not the current head, the provider refuses the entry with
  `conflict`, and returns the current head.
- `write` and `edit` send the head that the owner last read as `parent`.
  `--if-rev` names that head explicitly.
- `append` reads the head, adds the text, and writes. If the provider answers
  `conflict`, the client syncs and tries again. The client tries three times,
  then reports `conflict`.

The provider checks `parent` without reading the body. Concurrency control
therefore works on ciphertext.

## 9. KPIs

A KPI record is an entry of kind `kpi`. Its sealed body is one JSON object:

```json
{"t":"2026-09-11T15:40:00Z","key":"auc","value":0.871,"ref":"df62851","note":"lr=3e-4"}
```

- The KPI stream ID names the notebook, as section 5 shows.
- `kpi set` adds one KPI record. Nothing changes a KPI record after that.
- The client orders the KPI records of a stream by sequence, not by `t`. The
  client does not trust the clock of a machine to give order.
- `kpi log` returns the records for one key in sequence order.
- `kpi latest` returns the last record for each key of the notebook.
- If a page contains the line `<!-- kpi: auc -->`, `read` shows the latest
  value in its place. The page itself does not change.

## 10. Search

The client searches. The provider holds nothing that it can search.

1. The client syncs.
2. The client opens the head of each page stream in memory.
3. The client ranks the pages with BM25, with `k1 = 1.2` and `b = 0.75`.
4. The client drops each page with the tag `deleted`.
5. The client returns the address, the score, and one matching line of each
   page.

Tokens are lower-case runs of Unicode letters and digits. The title counts as
part of the body.

The client writes no plain text to disk. It opens the pages again for each
search. The design target is a few thousand pages. A sealed index on disk is a
later step.

## 11. Sync and the local copy

The client keeps a copy of the owner's entries:

```text
~/.config/arc/journal/<owner hex>/
  provider              the public key of the provider in use
  <provider hex>/
    cursor              the highest sequence that the client holds
    written             the highest sequence that `put` returned to this client
    heads.json          stream ID -> rev of the held head
    entries/<rev>       one entry, exactly as the provider sent it
```

Every entry on disk is sealed. The copy holds no address and no text in the
clear.

Each command that reads syncs first:

1. The client sends `since <cursor>`.
2. For each new sequence, the client fetches the entry with `get <rev>`.
3. The client checks each entry, in sequence order.
4. If every entry passes, the client stores them and moves the cursor.

The checks are:

- The sequences start at `cursor + 1` and have no gap.
- The log reaches the sequence in `written`.
- The SHA-256 hash of each entry is the rev that the log names.
- The signature is valid, and `author` is the owner.
- The client does not hold the rev already.
- For a page revision, `parent` is the head that the client holds for that
  stream. For a new stream, `parent` is empty.

If one check fails, the client stores nothing from that sync and reports
`untrusted_log` with the sequence and the check. The client does not repair
the log.

## 12. The provider

### 13.1 Storage

The provider uses plain files. It needs no database.

```text
<root>/
  <owner hex>/
    log                     one line per entry: <sequence> <kind> <stream ID> <rev>
    heads/<stream ID>       the rev of the head of one page stream
    entries/<rev>           one entry
```

The provider writes each file to a temporary name, then renames it. It holds
one lock for each owner while it checks a head, stores an entry, and adds a
log line.

### 13.2 Checks on `put`

The provider refuses an entry unless all of these are true:

- The entry is valid JSON with the fields of section 6, and no other field.
- `author` is the caller, which is the `from` key of the request.
- The signature is valid.
- For a page revision, `parent` is the current head of its stream.
- The owner stays under the byte quota after the entry.

The provider cannot read the body. It checks only the envelope.

### 13.3 Configuration

`JOURNAL_CONFIG` must name an absolute path to a JSON file:

```json
{
  "root": "/var/lib/arc-journal",
  "grants": ["<public key, 64 hex>"],
  "max_bytes": 1073741824
}
```

- `grants` lists the identities that can keep a journal on this provider. It
  must hold at least one key. If a caller is not in the list, the provider
  refuses with `forbidden`.
- `max_bytes` caps the entries of one owner. The default is 1 GiB.
- If `JOURNAL_CONFIG` is not set, or the file is not valid, the provider does
  not start.

## 13. Provider commands

The provider uses the request line format of every ARC provider. The first
line of `message` is the command. The text after the first newline is the
request body.

| Command | Body | Reply |
| --- | --- | --- |
| `put` | one entry | `rev <rev> sequence <n>` |
| `get <rev>` | none | one entry |
| `head <stream ID>` | none | a rev, or `not_found` |
| `since <sequence> [--limit n]` | none | one line per entry: `<sequence> <kind> <stream ID> <rev>` |
| `usage` | none | `entries <n> bytes <n> max <n>` |

`since` returns at most 1,000 lines. The default is 1,000.

A request always acts on the journal of its caller. No command names an
owner. One owner therefore cannot address the journal of another.

Errors are one word, then optional detail:

| Error | Meaning |
| --- | --- |
| `forbidden` | The caller is not in `grants`, or `author` is not the caller. |
| `conflict` | `parent` is not the current head. The detail is the current head. |
| `bad_signature` | The signature does not verify. |
| `invalid_entry` | The entry does not have the fields of section 6. |
| `not_found` | No entry or head has that name. |
| `too_large` | An entry is over its limit. |
| `quota_exceeded` | The owner is at `max_bytes`. |
| `unknown_command` | The command does not exist. |

## 14. Client commands

| Command | Purpose |
| --- | --- |
| `arc journal use <provider>` | Select the provider for this owner. |
| `arc journal status` | Show the provider, the cursor, and the usage. |
| `arc journal ls [<project>[/<notebook>]]` | List projects, notebooks, or pages, with titles. |
| `arc journal read <addr> [--lines a:b]` | Show a page or a range of lines, with its rev. |
| `arc journal write <addr> [--title t] [--tags a,b] [--if-rev r]` | Replace the page with the standard input. Create it if it does not exist. |
| `arc journal append <addr> <text>` | Add text to the end of the page. |
| `arc journal edit <addr> --if-rev r --find s --replace t` | Replace one string in the page. |
| `arc journal link <addr> <uri> [--name n] [--sha256 h]` | Record where a file lives. |
| `arc journal kpi set <project>/<notebook> <key> <value> [--ref r] [--note n]` | Add a KPI record. |
| `arc journal kpi log <project>/<notebook> <key>` | Show the records of one key. |
| `arc journal kpi latest <project>/<notebook>` | Show the last value of each key. |
| `arc journal search <query> [--project p] [--notebook n]` | Search the pages. |
| `arc journal history <addr>` | List the revisions of a page. |
| `arc journal import <provider> [--files dir]` | Copy a journal from a provider of the earlier design. |

Output rules:

- Every answer is short by default. `ls` returns the address and the title.
  `search` returns the address, the score, and one matching line.
- To see more, the owner calls `read` with a range of lines.

## 15. Where the client lives

The journal logic runs in `arc`, not in the provider. A manifest template
cannot derive a key, sync a log, or rank search results. `arc journal` is
therefore a command group of `arc` itself, like `arc cache` and `arc update`.

The client code lives in the `journal` package, so another program can import
it. The provider manifest still announces the capability, so `arc discover`
finds the provider. `journal` is a reserved command name, so an install cannot
take it.

## 16. Limits

| Limit | Value | Reason |
| --- | --- | --- |
| Request or reply | under 1 MiB | The ARC client refuses a larger request body. |
| Page body | 512 KiB of plain text | A sealed page, as base64 in an entry, stays under 1 MiB. |
| `since` answer | 1,000 lines | A bound on one reply. |
| `append` attempts | 3 | A bound on the retry after `conflict`. |
| Quota | 1 GiB for each owner, by default | Set by the operator in `max_bytes`. |

## 17. Moving from the earlier design

`arc journal import <provider>` copies a journal from a provider of the
earlier design:

1. The client lists every page that the owner can read, with `ls`.
2. For each page, the client reads the current text and writes it as the first
   revision of a new page stream.
3. For each notebook, the client reads each KPI key with `kpi log`, and adds
   the records in order.
4. For each attachment, the client fetches the bytes and writes them to a
   directory that the owner names with `--files <dir>`. Then it replaces the
   attachment with a `file://` link to that copy.

The import does not copy history. The earlier provider returns the current
text of a page only, so each page starts with one revision.

A project of the earlier design can have more than one reader. The import
copies such a project into the private journal of the owner who runs it. The
other readers keep their own copies on the earlier provider.

The earlier provider returns an attachment in one reply, as base64. An
attachment of 16 MiB becomes a reply of about 21.4 MiB. The import fails for
such an attachment if the relay caps frames below that size.

If the owner does not give `--files`, the import stops before it writes, and
lists each attachment that it cannot carry.

The import leaves the earlier provider unchanged.

## 18. Open questions

- Hard deletion. An owner can ask the provider to erase every entry of their
  journal. The design does not define this command yet.
- A sealed search index on disk, when a journal grows past a few thousand
  pages.
- Backup of the provider itself. Each synced client holds a full copy, so the
  design adds no separate backup.
