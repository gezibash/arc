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
skipping missing or deleted pages. Page edits maintain the notebook index.
`index` rebuilds it. Index and ToC references include the identity and can name
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
