# Journal app

Journal adds local commands over encrypted Markdown pages. Its manifest runs
through ARC's shared application runtime; it needs no dedicated server program.
The manifest author does not receive the pages or their encryption key. ARC can
sync the encrypted events through a relay or a carried directory.

## Install its commands

Install the journal interface from an author you trust:

```sh
arc install <manifest-author> journal
arc help journal
arc apps info journal
```

To announce this checkout's interface under your own identity, use
`arc announce apps/journal/manifest.json`, then install `journal` from the public
key printed by `arc whoami`. Announcing publishes a signed interface on your
configured delivery paths.

## Write and read a page

Addresses are `project/notebook/page`. Page numbers are positive ordinals.
Required front matter names the title, notebook and page. ARC supplies UTC
`created_at` and `updated_at` values and retains the original creation time.

```sh
arc journal write arc/design/1 <<'MD'
---
title: App boundaries
notebook: arc/design
page: 1
---
# Decision
Apps own domain behavior. Core owns ARC protocols.
MD

arc journal read arc/design/1
arc journal append arc/design/1 Follow up with the SQLite REPL.
arc journal ls --notebook arc/design --order page
arc journal index arc/design
arc journal toc arc/design
```

After another page exists, `next` and `prev` move by ordinal within its notebook,
skipping missing or deleted pages. `index` and `toc` read the pages of the
notebook each time. Index and ToC references include the identity and can name
a Markdown heading; `read --section Decision` selects that section.

```sh
arc journal select --notebook arc/design --from 2026-09-01 --to 2026-09-30
arc journal search boundaries --notebook arc/design
arc journal search 'title:"App boundaries"' --syntax
arc journal ls --notebook arc/design --json | jq -r '.tags.d'
```

Check `arc help journal` for all filters and output options. Structured output
can be consumed with ordinary shell tools.

## Storage and search

Encrypted events are authoritative and live in the identity's event store.
Markdown is decrypted for authorized local commands. The private persistent
Bleve index holds readable search terms and positions, though it does not store
the full page bodies. It is a local search cache, not the journal database or a
published service. The search adapter stays in `adapters/search/bleve`.

## Reference

The [manifest](manifest.json) defines every command of the journal. The
[app interface](../../docs/interface/SPEC.md) defines the primitives that it
uses.

A page is a NIP-23 article, kind 30023, inside a NIP-37 draft. The article's
`d` tag is the page address, sealed inside the draft; the draft's `d` tag is a
keyed value of the address. A client that knows NIP-37 opens the page as a
draft article. The checkpoints of the draft are the history of the page.

The journal is a collection of notebooks. A notebook is namespaced as
`project/notebook`, such as `arc/architecture`. Each new page has a positive
integer ordinal, for example `arc/architecture/1`. Page numbers are stable and
scoped to the notebook. Gaps are allowed; deleting page 2 never renumbers page
3. Old pages with names remain readable and appear as legacy entries; they are
not silently renumbered.

The input header requires `title`, `page` and `notebook`. Their combined address
must match the command address. ARC fills in `created_at` and `updated_at` as
UTC RFC3339 timestamps. `created_at` stays fixed through replacement and append;
`updated_at` records the latest write. Caller-supplied timestamps cannot change
these values. For an older numbered page with no creation field, rewriting uses
the earliest checkpoint available to this machine. UTC dates come from the
writer's clock; event replacement still uses core's monotonically newer time.

```sh
arc journal write arc/architecture/1 <<'MD'
---
title: Session boundaries
page: 1
notebook: arc/architecture
---
# Session boundaries

## Decision
Interaction behavior belongs in core.

## Evidence
The CLI session tests pass.
MD
arc journal read arc/architecture/1
arc journal next arc/architecture/1
arc journal prev arc/architecture/3
arc journal ls --notebook arc/architecture --from 2026-09-01 --to 2026-09-30
arc journal ls --notebook arc/architecture --order page --reverse --limit 10
arc journal select --notebook arc/architecture --page 1 --section decision
arc journal search --notebook arc/architecture --from 2026-09-01 session
arc journal index arc/architecture
arc journal toc arc/architecture
```

`ls` defaults to creation time, oldest first. `--order` selects `created`,
`updated`, or numeric `page`; `--reverse` reverses it. Filters can select an
exact notebook, page number, title substring, and inclusive creation date
bounds. `--from` and `--to` accept a UTC date or RFC3339 timestamp; a date for
`--to` includes that whole day. `--limit` is positive. `select` reads the full
selected pages, or the requested Markdown section. Search filters before
ranking and limiting. Search options must precede the search terms.

Journal search uses a local Bleve Scorch index with BM25 scoring. Plain queries
match any Unicode word, without stemming or stop-word removal. `--syntax` enables
Bleve query syntax: quoted phrases, `+required` and `-excluded` terms, fuzzy terms
such as `bird~1`, wildcards such as `bird*`, and `title:` or `text:` field searches.
`AND`, `OR`, and parentheses are not Boolean operators in this syntax. Use `+`
and `-` for required and excluded terms.

```sh
arc journal search --syntax --notebook arc/architecture '"bounded flow control"'
arc journal search --syntax '+session -http'
arc journal search --syntax 'title:architecture'
arc journal search --syntax 'bluetooh~1'
```

Each identity has a private derived index at
`<citizen-home>/store/search/journal-v1.bleve`. It contains readable search terms
and positions. It does not store full page bodies or go to relays. The source
pages remain encrypted ARC events in `events.db`. Protect the local index like
other private user data; filesystem permissions are not encryption at rest.

Before each search, ARC reads current verified page headers, applies metadata
filters, and indexes only selected pages whose content version changed. Unchanged
multipart bodies are not fetched again for ranking. `--json` reads the complete
Markdown of returned hits to preserve the search record format. Deleted pages
are removed. A filtered search does not fetch bodies from other notebooks. The first search on a new
machine builds its own index from synced encrypted pages. Updates received out
of order are detected by content version, not a wall-clock cursor. If a changed
selected page has a missing part, or an index update fails, search returns an
error instead of stale results.
The index can be rebuilt from source events; it does not replace ARC persistence.

`next` and `prev` read the closest higher or lower page number in the same
notebook. They skip gaps and deleted pages, and report a boundary when no page
exists. Chronological views and ordinal navigation are distinct operations.

`index` and `toc` build the notebook index from the verified source pages each
time. The index contains page numbers, titles, creation and update timestamps,
page references, and a table of Markdown headings. Pages from another machine
appear after a sync. `index` also keeps the result as an encrypted snapshot
through the normal draft/checkpoint core; `toc` keeps nothing. A write, an
append and a delete do not change the snapshot. If they did, each write would
store the whole index again, and the stored bytes would grow with the square of
the page count.

ToC links use `journal+arc://<own-public-key>/<project>/<notebook>/<page>#<anchor>`.
Pass a link to `arc journal read` to read the page or section. An address with
`#<anchor>` also works. Sections include their child headings and end at the
next heading of the same or higher level. ATX and Setext headings are supported;
fenced and indented code is omitted. Duplicate anchors receive numeric suffixes.
A journal reference must name the local identity. This does not grant access to
another identity's private pages or register an OS/browser URL handler.

The operator must announce the journal manifest to activate these
commands. Upgrade ARC first: older binaries reject its manifest fields.
The Markdown index uses the already-consented sealed article kind 30023.
Its two-component notebook address keeps it outside the three-component page
views, and its event kind keeps it outside KPI JSON queries.
Existing consent keeps the same event kinds and visibility.

A KPI series is one draft per notebook and key, around an event of kind 30078,
which NIP-78 defines for the data of one application. The draft holds the
latest value, and its checkpoints hold every value before it.
