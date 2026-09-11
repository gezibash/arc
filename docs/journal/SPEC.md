# Journal: a scientific notebook for agents on ARC

## 1. Purpose

The journal is an ARC provider. Agents write research notes, attach files, and
record KPIs. Each agent identifies with its ARC keypair. The journal stores
pages as markdown files in a git repository and indexes them with qmd.

The journal is a standalone provider bundle. It does not change ARC core.

## 2. Identity and access

- The ARC exec runtime gives the caller's public key in the `from` field of
  each request. The journal uses this key as the author of every write.
- A project has an `ACL` file. The file lists one public key per line.
- The first key that creates a project becomes the owner. The owner can edit
  the `ACL` file through the `acl` command.
- If the caller's key is not in the project `ACL`, the journal rejects writes
  with `error: forbidden`.
- Reads are open to all keys in the `ACL`. Public read is not in v1.

## 3. Address model

Every page has an address: `<project>/<notebook>/<page>`.

- `project`: a research effort. Example: `hrs`.
- `notebook`: a topic inside the project. Example: `ablations`.
- `page`: one document. Example: `2026-09-11-lr-sweep`.

Each segment must match `[a-z0-9][a-z0-9-_.]*`. The journal rejects other
characters.

## 4. Storage layout

All data lives under `JOURNAL_ROOT`. The default is `~/.arc/journal`.

```
JOURNAL_ROOT/
  repo/                                  git repository
    projects/<project>/ACL
    projects/<project>/<notebook>/<page>.md
    projects/<project>/<notebook>/kpi.jsonl
  blobs/<sha256>                         attachments, content addressed
  .qmd/                                  search index, derived, not backed up
```

Rules:

- The journal makes one git commit per write. The commit author is the
  caller's public key.
- Attachments are not in git. The journal stores them in `blobs/` and
  references them by hash from the page.
- The `.qmd` index is derived data. The journal rebuilds it from `repo/`.

## 5. Page format

A page is a markdown file with YAML frontmatter.

```markdown
---
title: LR sweep on HRS
created: 2026-09-11T14:02:11Z
author: 9f3a...c1        # public key, hex
updated: 2026-09-11T15:40:00Z
tags: [ablation, lr]
refs:
  - github: owner/repo@df62851
  - commit: df62851
attachments:
  - name: plot.png
    sha256: 4a1c...
    bytes: 88112
links:
  - name: run-dir
    uri: file:///Users/zim/runs/lr-sweep
    host: 9f3a...c1        # public key of the machine that owns the path
  - name: checkpoint
    uri: s3://hrs-runs/lr-sweep/ckpt-1200.pt
    sha256: 7be0...
---

Body in markdown.
```

The journal owns `created`, `author`, `updated`, `attachments`, and `links`.
Callers own `title`, `tags`, and `refs`.

## 6. Revisions and concurrency

- Every read returns `rev`, the short git SHA of the last commit that touched
  the page.
- `write` and `edit` accept `--if-rev <rev>`. If the current `rev` differs,
  the journal rejects the write with `error: conflict` and returns the current
  `rev`.
- `append` never checks `rev`. It adds text to the end of the page.
- Callers that edit prose must use `--if-rev`. Callers that add notes must use
  `append`.

## 7. KPIs

KPIs are not page edits. KPIs live in `kpi.jsonl` in each notebook.

One line per record:

```json
{"t":"2026-09-11T15:40:00Z","by":"9f3a...c1","key":"auc","value":0.871,"ref":"df62851","note":"lr=3e-4"}
```

- `kpi set` appends one line. The journal never rewrites the file.
- `kpi log` returns the records for one key in time order.
- `kpi latest` returns the last record for each key in the notebook.
- If a page contains the line `<!-- kpi: auc -->`, `read` replaces it with the
  latest value on output. The file on disk does not change.

## 8. Attachments

- `attach` sends the file body as base64 in `--base64`. The journal hashes
  the body, stores it in `blobs/`, and adds an entry to the page frontmatter.
- v1 caps attachments at 16 MiB. If the body is larger, the journal rejects
  the request with `error: too_large`. The cap is a storage budget: blobs
  live outside git and v1 does not back them up. It is not a transport limit.
  The ARC exec port reads provider reply lines up to 64 MiB, so a `fetch` of
  the largest blob, 21.4 MiB as base64, fits on one line.
- A `fetch` reply that crosses a relay is one frame. The relay frame cap,
  `max_frame_bytes` in `arc_net`, is 4 MiB by default. Raise it on both ends
  to fetch blobs over 3 MiB through a relay.
- `fetch` returns the blob body as base64 by hash.

### Links

A link records where a file lives. The journal stores no bytes for a link.

- `link` adds one entry to `links` in the page frontmatter.
- `uri` is any URI: `file://`, `https://`, `s3://`, `arc://`, or a
  `github:` reference.
- If the URI scheme is `file://`, the journal sets `host` to the caller's
  public key. The path is only valid on that host.
- If the caller passes `--sha256`, the journal stores it. The journal does not
  verify it.
- `read` returns links as they are stored. The journal never follows a link.
- If the caller needs the bytes later, the caller resolves the link. A host
  that serves its files as an ARC capability makes `arc://` links resolvable
  by any agent with access.

## 9. Search

- The journal runs `qmd search <query>` for the `search` command. This is
  BM25 and needs no model.
- If the caller passes `--deep`, the journal runs `qmd query <query>`.
- The journal scopes the search with `--project` and `--notebook` filters
  applied to the returned paths.
- After each commit, a background job runs `qmd update`. Writes do not wait
  for the index.

## 10. Commands

The manifest exposes a CLI interface with namespace `journal`. Each command
maps to one request line on the wire.

| Command | Purpose |
|---|---|
| `ls [<project>[/<notebook>]]` | List projects, notebooks, or pages. One line each. |
| `read <addr> [--lines a:b]` | Return a page or a line range with `rev`. |
| `write <addr> [--title t] [--tags a,b] [--if-rev r] [--body <text>]` | Replace the page body with the request body, or with `--body`. Create if absent. |
| `append <addr> <text>` | Add text to the end of the page. |
| `edit <addr> --if-rev r --find s --replace t` | Replace one string in the body. |
| `attach <addr> --name <n> --base64 <b>` | Store a blob and link it. |
| `link <addr> <uri> [--name n] [--sha256 h]` | Record a reference. Store no bytes. |
| `fetch <sha256>` | Return a blob body. |
| `kpi set <project>/<notebook> <key> <value> [--ref r] [--note n]` | Append a record. |
| `kpi log <project>/<notebook> <key>` | Return the history of one key. |
| `kpi latest <project>/<notebook>` | Return the last value of each key. |
| `search <query> [--project p] [--notebook n] [--deep]` | Search pages. |
| `acl <project> add\|rm\|ls [pubkey]` | Edit or list the project ACL. Owner only for add and rm. |
| `history <addr>` | List revisions of a page. |

Output rules:

- Every response is small by default. `ls` returns the address and the title.
  `search` returns the address, the score, and one matching line.
- If the caller wants more, the caller calls `read` with a line range.
- Errors are one word, then optional detail: `forbidden`, `conflict`,
  `not_found`, `too_large`, `invalid_address`, `invalid_number`,
  `unknown_command`. The ARC CLI prefixes them with `error:` on display.

## 11. Wire format

The ARC exec runtime sends one JSON object per line on stdin:

```json
{"op":"request","message":"<command line>","from":"<hex pubkey>","meta":{},"request_id":"..."}
```

The first line of `message` is the command line. Any text after the first
newline is the request body. `write` stores it as the page body, byte for
byte, when the command line has no `--body`. A request that sets both is
rejected with `invalid_arguments`. The ARC CLI builds this shape from
`input.source: "stdin"`: it renders the header from the parsed options and
appends the standard input after a newline, so the body never touches the
shell's argument limit.

The manifest renders every free-text option (`--body`, `--title`, `--tags`,
`--if-rev`, `--find`, `--replace`, `--note`) with the `{{key|json}}` template
filter, so the value arrives as a JSON string literal and the journal decodes
it. Quotes, newlines, and ` --words` inside a value survive unchanged. An
absent option renders as the bare word `null`, which the journal treats as
not given. Other quoted values run to the last quote before the next
` --flag` or the end of input. The literal two characters `\n` in a `--body`
value or in append text become a newline. The journal replies with one JSON
object per line on stdout:

```json
{"op":"reply","request_id":"...","reply":"<text>"}
{"op":"error","request_id":"...","error":"<word> <detail>"}
```

## 12. Runtime

- Language: Elixir. The bundle at `providers/journal` is a small Mix project.
- `run.sh` builds an escript on first start and runs it. Build output goes to
  stderr so stdout stays a clean JSON stream.
- One OTP supervisor runs the index job and the push job. The stdio loop runs
  in the main process.
- The journal shells out to `git` and `qmd`. `git` must be on `PATH`. If
  `qmd` is absent, `search` returns `search_unavailable` and all else works.
- The data repo sets `core.hooksPath` to `/dev/null`, so global git hooks
  never run on journal commits.
- Configuration comes from environment variables:
  - `JOURNAL_ROOT`: data directory. Default `~/.arc/journal`.
  - `JOURNAL_REMOTE`: git remote URL. If set, the push job pushes after each
    commit with a 30 second debounce.

## 13. Backup

- If `JOURNAL_REMOTE` is set, the git repository is the backup for pages and
  KPIs.
- Blobs are not in git. v1 does not back up blobs. A later version adds an
  S3 sync.
- To restore: clone the remote into `JOURNAL_ROOT/repo`, then start the
  provider. The index rebuilds on first start.

## 14. Non-goals for v1

- Two hosts that serve one journal.
- Public read access.
- Rich text or a web UI.
- Attachments larger than 16 MiB.
- Deleting pages. Use `write` with an empty body and a `deleted: true` tag.

## 15. Resolved questions

- The ARC exec port once cut provider reply lines at 1 MB, which set the
  attachment cap at 512 KiB so a base64 `fetch` reply would fit. The exec
  port now joins stdout chunks up to 64 MiB per line. The attachment cap is
  16 MiB and follows the storage budget, see section 8.
- Template inputs escape values with the `{{key|json}}` filter, so a value
  may contain quotes, newlines, and ` --flag`-like words without being cut.
  The `write` body travels as the request body after the header line
  (`input.source: "stdin"`), so its size limit is the provider's, not the
  shell's argument limit. Short bodies may still use `--body <json string>`.

## 16. Verified

Run end to end on 2026-09-11 through a local relay: install as a second key,
write, kpi set, read with KPI injection, ls, link, append, history, search
through qmd, ACL denial for a third key, ACL grant, and read after grant.
