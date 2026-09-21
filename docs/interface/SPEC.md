# Capability interface, version 1

Status: proposed. Nothing in this document is built. It replaces the command
line interfaces of the older stack, versions 1 to 4. Those interfaces needed
code in core for direct messages, Agora, and files.

## 1. Purpose

A capability tells `arc` which commands it adds, and what each command does.
The capability says this in its manifest. The manifest names primitives, and
core runs them. The manifest holds no code.

Two rules follow:

- **A new capability needs a manifest, and nothing in core.** The primitives
  are the same for every capability. A journal, a board, and a file store are
  manifests over the same primitives.
- **A manifest cannot make a citizen's key do anything outside the
  primitives.** Core owns every primitive. A provider cannot run code on the
  caller's machine.

This interface runs on the delivery layer of docs/delivery/SPEC.md. Every
datum is a signed event, so signing and verification are not primitives: the
delivery layer does both for every event.

## 2. Two shapes

| Shape | What answers | Example |
| --- | --- | --- |
| service | A provider program answers calls. | exec, sqlite, releases |
| data | Nobody answers. The citizen writes events and reads them. | journal, direct messages, Agora, files |

A data capability has no provider. Its manifest still comes from an author,
who signs its announcement. A citizen installs the manifest by trusting that
author.

## 3. Terms

| Term | Meaning |
| --- | --- |
| manifest | The JSON document that defines a capability. |
| author | The citizen who signs the announcement of a manifest. For a service, this is the provider. |
| command | One entry that the manifest adds to `arc`. |
| action | What a command does: call, publish, query, or watch. |
| record | One item that the output pipeline works on. |
| pipeline | The output primitives of a command, in their fixed order. |
| kind name | A name that a manifest gives to one event kind, in its `kinds` section. |

## 4. The manifest

```json
{
  "interface": 1,
  "id": "journal",
  "title": "Journal",
  "summary": "A private notebook, sealed to your own key.",
  "shape": "data",
  "kinds": {
    "head": {"kind": 30078, "visibility": "sealed"},
    "part": {"kind": 3275, "visibility": "sealed"}
  },
  "formats": { },
  "commands": [ ]
}
```

| Field | Meaning |
| --- | --- |
| `interface` | Must be 1. |
| `id` | The capability id. The announcement uses it as its `d` tag. |
| `title`, `summary` | Shown by `discover` and at install. |
| `shape` | `service` or `data`. |
| `kinds` | Every kind that the commands publish or read, by name. See 4.1. |
| `service` | For a service: the default method, path, and body limit. See 4.2. |
| `formats` | Named ways to show records. See section 10. |
| `commands` | The commands. See 4.3. |

### 4.1 Kinds

Each entry names one event kind and its visibility:

| Visibility | Meaning |
| --- | --- |
| `public` | The content is readable by everyone. |
| `sealed` | The content is sealed to the author's own key with NIP-44. The event is signed and public; its content is not. |
| `private` | The event is a rumor inside a gift wrap, for named recipients, as NIP-59 defines. The mail layer carries it. |

A command can publish only kinds that the manifest names here. Section 12
lists the kinds that no manifest can name.

### 4.2 Service

```json
"service": {"method": "EXEC", "path": "/", "max_bytes": 1048576}
```

A command of a service can override the method and the path.

### 4.3 Commands

```json
{
  "path": ["write"],
  "summary": "Replace a page with the standard input",
  "args": [
    {"name": "address", "kind": "positional", "type": "address", "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"},
    {"name": "title", "kind": "option", "type": "text"}
  ],
  "action": {"publish": { }},
  "output": {"format": "written"}
}
```

`path` is the words of the command after the capability's name. An empty path
is the capability's name alone. Every command has exactly one action.

## 5. Arguments

| Kind | Meaning |
| --- | --- |
| `positional` | In order. The last positional can set `"variadic": true`. |
| `option` | `--name value`. |
| `switch` | `--name`, with no value. |

| Type | Meaning |
| --- | --- |
| `text` | Any text. A variadic text joins its words with one space. |
| `integer` | A whole number. |
| `key` | A citizen. Core resolves 64 hex characters, a petname of this machine, or an installed name, to a public key. |
| `file` | A local path that core reads. |
| `path` | A local path that core writes to. It must not exist. |
| `address` | Text that must match the argument's `pattern`. |
| `lines` | A range of lines: `a:b`, `a:`, or `:b`. |
| `stdin` | The standard input. A command has at most one. |

An argument can set `"required": true`, and a `"default"`.

## 6. Templates

A value in an action can hold placeholders: `{{name}}`, where `name` is an
argument. A filter changes the value: `{{name|json}}`. Filters apply from left
to right.

| Filter | Result |
| --- | --- |
| `json` | The value as a JSON literal. An absent value is `null`. |
| `hex` | The value as lower-case hex. |
| `keyed:<purpose>` | An HMAC of the value, as 64 hex characters. See 6.1. |
| `coord:<kind name>` | The coordinate of an addressable event of this citizen: `<kind>:<own public key>:<value>`. |
| `event_author` | The author of the event whose ID is the value. Core reads the event from the store, then from the transports, and fails the command when it finds none. |
| `default:<text>` | The text, when the value is absent. |

Each template also knows these names:

| Name | Value |
| --- | --- |
| `me` | The citizen's public key. |
| `author` | The public key of the capability's author. |
| `now` | The time now, as seconds since 1970. |

### 6.1 Keyed values

A keyed value names something without revealing it. A journal page, for
example, is found by a keyed value of its address:

```text
key   = HKDF-SHA256(ikm = seed, salt = "",
                    info = "arc-keyed-v1" || 0x00 || author || 0x00 || capability id || 0x00 || purpose,
                    length = 32)
value = hex(HMAC-SHA256(key, input))
```

The key depends on the author and the capability id. One capability therefore
cannot compute the keyed values of another.

## 7. Actions

### 7.1 call

Only a service has `call`. It sends one request to the provider, as
docs/delivery/SPEC.md section 11.4 defines.

```json
"call": {
  "class": "live",
  "method": "EXEC",
  "path": "/",
  "body": "{\"argv\": {{argv|json}}}"
}
```

| Field | Meaning |
| --- | --- |
| `class` | `live` or `later`. A live call needs a live path now. A later call waits in the outbox. `--later` on the command line changes live to later. |
| `method`, `path` | Override the service defaults. |
| `body` | A template, `{{stdin}}`, or `{{file}}` for a `file` argument. |

The reply becomes one record. The output pipeline says how to read it.

### 7.2 publish

`publish` makes one event, signs it, and sends it.

```json
"publish": {
  "kind": "head",
  "d": "{{address|keyed:page}}",
  "content": {"json": {"address": "{{address}}", "title": "{{title}}"}},
  "tags": [["t", "journal"]],
  "parts": {"kind": "part", "from": "{{stdin}}", "size": 32768, "max": 256},
  "to": ["{{recipient}}"],
  "relays": "own"
}
```

| Field | Meaning |
| --- | --- |
| `kind` | A kind name from `kinds`. The visibility comes from there. |
| `d` | For an addressable kind, the `d` tag. |
| `content` | A template for text, or `{"json": ...}` for an object whose values are templates. |
| `tags` | Tags, whose values are templates. |
| `parts` | Split a large body into part events. See 7.2.1. |
| `to` | For a private kind: the recipients, as `key` arguments. The author keeps a copy in its own seal. |
| `relays` | `own`, the citizen's relays, or `author`, the relays that the capability's author lists in NIP-65. Private events also go to each recipient's NIP-17 relay list. |

A private event goes through the mail layer. It waits in the outbox, travels
with couriers, and is acknowledged, as docs/delivery/SPEC.md section 10
defines.

#### 7.2.1 Parts

A relay caps the size of one event, so a large body travels as parts:

1. Core reads the body a part at a time, and cuts each part at a line end, at
   most `size` bytes. A line longer than a part is cut inside the line.
2. Core publishes each part as an event of the part kind, with an `a` tag that
   names the coordinate of the main event.
3. Core publishes the main event last. Its content gains a field `parts`: the
   IDs of the parts, in order, with the bytes and lines of each.

A reader therefore never sees a main event whose parts were not sent. The
default `size` is 32768, so a sealed part stays under the 64 KiB limit of
common relays. The default `max` is 256 parts.

### 7.3 query

`query` reads events from the store, and first asks the transports for any
that the store lacks.

```json
"query": {
  "kinds": ["head"],
  "authors": "me",
  "tags": {"d": "{{address|keyed:page}}"},
  "limit": 1
}
```

| Field | Meaning |
| --- | --- |
| `kinds` | Kind names from `kinds`. |
| `authors` | `me`, `author`, `any`, or a template that names a `key` argument. |
| `tags` | Tag filters, whose values are templates. An empty value drops the filter. |
| `ids` | A template that names event IDs. |
| `since`, `until`, `limit` | As NIP-01 defines. |

Each value in a query can be a template.

A query of a private kind reads the messages that this citizen received and
sent, from their seals.

### 7.4 watch

`watch` is a query that stays open. It runs the pipeline on each event that
matches, first on the stored events, then on each new one as it arrives. It
ends when the citizen stops it, or when every relay ends the subscription.

## 8. Records

The pipeline works on records. Each event becomes one record with these
fields:

| Field | Value |
| --- | --- |
| `id` | The event ID. |
| `kind` | The kind name. |
| `author` | The public key of the author. |
| `created` | The time of the event. |
| `tags` | The first value of each tag, by tag name. |
| `content` | The content, after `open`. |

`open` adds the fields of parsed content. `join` adds `text`. A reply to a call
becomes one record whose `content` is the reply, and whose `error` is set when
the provider refused.

## 9. The output pipeline

A command lists its output primitives. Core runs them in this order, whatever
the order in the manifest:

| Order | Primitive | What it does |
| --- | --- | --- |
| 1 | `open` | Opens sealed content and private events, and parses content as `text`, `json`, or `frontmatter` into fields. |
| 2 | `join` | Joins the parts of each record into `text`. See 9.1. |
| 3 | `where` | Keeps records whose fields match. |
| 4 | `latest` | Keeps the newest record for each value of a field. |
| 5 | `rank` | Orders records by BM25 against a query, over named fields. |
| 6 | `thread` | Orders records as replies, by a field that names the parent. |
| 7 | `sort`, `limit` | Orders by a field, and cuts. |
| 8 | `save` | Writes a field to a `path` argument, instead of showing it. |
| 9 | `format` | Shows each record with a named format, see section 10. |

```json
"output": {
  "open": {"parse": "json"},
  "join": {"lines": "{{lines}}"},
  "format": "page"
}
```

### 9.1 join

`join` needs records with a `parts` field. For each record, it:

1. Fetches the listed parts from the store, then from the transports.
2. Adds each part that names the record's coordinate in its `a` tag, and is
   newer than the record, in time order. An append therefore needs no rewrite
   of the main event.
3. Opens each part, and writes its text in order.

If a listed part is missing, `join` stops after the text before it, and
reports the missing part. With `lines`, `join` fetches only the parts that hold
those lines. When `join` is the last primitive before `format`, it writes each
part as it arrives.

### 9.2 where, latest, rank, thread, sort

```json
"where":  [{"field": "address", "prefix": "{{path}}"}, {"field": "tags.t", "lacks": "deleted"}],
"latest": {"by": "key"},
"rank":   {"query": "{{query}}", "fields": ["title", "text"], "limit": 20},
"thread": {"parent": "tags.e"},
"sort":   {"field": "created", "order": "asc"}
```

A condition whose value template is empty is dropped, so an optional argument
can narrow a query or leave it whole.

`rank` uses BM25 with `k1 = 1.2` and `b = 0.75`, over lower-case runs of
Unicode letters and digits. It adds a field `score`.

## 10. Formats

A format renders records as text:

```json
"formats": {
  "page": {"record": "{{text}}"},
  "list": {"record": "{{address}}\t{{title}}\t{{created|date}}", "empty": "no pages"},
  "rows": {"table": {"columns": "columns", "rows": "rows"}}
}
```

| Field | Meaning |
| --- | --- |
| `header` | A template shown once, before the records. |
| `record` | A template shown for each record. |
| `empty` | Shown when there is no record. |
| `table` | Shows a record's rows as aligned columns, instead of `record`. |

Format templates have these helpers: `time`, `date`, `petname`, `short`,
`indent`, `truncate:n`, and `default:<text>`.

Core removes every control character except newline and tab from each value
before it shows it. A value from an event cannot move the cursor, or change
the terminal.

## 11. Flags of every command

| Flag | Meaning |
| --- | --- |
| `--json` | Writes each record as one JSON object on one line, after the pipeline and before `format`. Agents read this. |
| `--dry-run` | For `publish`: writes each event that core would sign, and signs nothing. |
| `--later` | For a live `call`: waits in the outbox instead. |

## 12. Security

**Install shows what a capability can do.** Before a citizen trusts a
capability, `arc install` shows its author, its shape, and each kind that it
publishes, with the visibility of each. A capability that publishes a public
kind can publish under the citizen's name, and the install says so.

**Some kinds are reserved.** No manifest can name these kinds, because they
speak for the citizen's identity, or move its control:

| Kind | Why |
| --- | --- |
| 0, 3 | Profile and follows. |
| 5 | Deletion of other events. |
| 13, 1059, 21059 | Seals and gift wraps. Only the mail layer makes them. |
| 3272, 3273, 3274 | Calls and acknowledgements. Only core makes them. |
| 9734, 9735 | Zaps. |
| 10002, 10050 | Relay lists. |
| 10272, 30272 | Migration records and announcements. |
| 13194, 23194, 23195 | Wallet connect. |
| 22242, 24133, 27235 | Authentication and remote signing. |

**Recipients come from the citizen.** A private event goes only to keys that
the citizen typed as `key` arguments, and to the citizen's own seal. No value
from an event or a reply can add a recipient.

**Files come from the citizen.** Core reads a file only when the citizen typed
its path as a `file` argument. Core writes a file only to a `path` argument
that does not exist, and never replaces a file.

**Keyed values stay inside a capability.** See 6.1.

**A reply is data.** Core never runs, opens, or follows what a reply or an
event holds.

## 13. Install and dispatch

`arc install <author> [capability]` reads the announcement, verifies it, shows
what section 12 requires, and asks the citizen once. The capability's name is
its id, unless the citizen gives another with `--as`.

`arc <name> <path...> [args]` finds the installed capability, then the command
with the longest matching path, and runs it. `arc help <name>` lists the
commands.

A new version of a manifest replaces the old one when its author announces
it. If the new version publishes a kind that the old one did not, or makes a
kind more visible, core asks the citizen again before it runs a command.

## 14. Versions

This interface is version 1. Core refuses a manifest of a later version, and
says which version it would need. A new primitive, or a new field with a new
meaning, comes in a new version. The older stack's interfaces, versions 1 to
4, do not run on the delivery layer.

## 15. What leaves core

| Today | In version 1 |
| --- | --- |
| The `journal` package | The journal manifest, over `publish`, `query`, `watch`, parts and `join`. |
| The direct-message renderers and cache in `toolbox` | The dm manifest, over private kinds and the mail layer. |
| The `agora` input source | The agora manifest, over public kinds 11 and 1111. |
| The `private_file` and `sealed_file` inputs | The files manifest, over sealed parts. |
| The `seal`, `pubkey` and `shell` filters | `key` arguments, private kinds, and `json`. |

## 16. The capabilities in version 1

### 16.1 exec

```json
{
  "interface": 1, "id": "exec", "shape": "service",
  "title": "Exec", "summary": "Runs commands for the citizens that it grants.",
  "service": {"method": "EXEC", "path": "/", "max_bytes": 1048576},
  "kinds": {},
  "formats": {
    "run": {"record": "{{stdout}}{{stderr}}"},
    "job": {"record": "{{job}}\t{{state}}"}
  },
  "commands": [
    {"path": ["run"], "summary": "Run one command",
     "args": [{"name": "argv", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"call": {"class": "live", "body": "{\"argv\": {{argv|json}}}"}},
     "output": {"open": {"parse": "json"}, "format": "run"}},
    {"path": ["start"], "summary": "Start a script as a job",
     "args": [{"name": "script", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"call": {"class": "later", "body": "{\"action\": \"start\", \"script\": {{script|json}}}"}},
     "output": {"open": {"parse": "json"}, "format": "job"}},
    {"path": ["status"], "summary": "Show a job",
     "args": [{"name": "job", "kind": "positional", "type": "text", "required": true}],
     "action": {"call": {"class": "live", "body": "{\"action\": \"status\", \"job\": {{job|json}}}"}},
     "output": {"open": {"parse": "json"}, "format": "run"}}
  ]
}
```

### 16.2 sqlite

```json
{
  "interface": 1, "id": "sqlite", "shape": "service",
  "title": "SQLite", "summary": "Answers SQL for the citizens that it grants.",
  "service": {"method": "QUERY", "path": "/main", "max_bytes": 1048576},
  "kinds": {},
  "formats": {"rows": {"table": {"columns": "columns", "rows": "rows"}}},
  "commands": [
    {"path": [], "summary": "Run SQL",
     "args": [{"name": "sql", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"call": {"class": "live", "body": "{\"sql\": {{sql|json}}}"}},
     "output": {"open": {"parse": "json"}, "format": "rows"}}
  ]
}
```

### 16.3 releases

```json
{
  "interface": 1, "id": "releases", "shape": "service",
  "title": "Releases", "summary": "Serves signed release channels and their archives.",
  "service": {"method": "RAW", "path": "/releases", "max_bytes": 4096},
  "kinds": {},
  "formats": {"channel": {"record": "{{content}}"}},
  "commands": [
    {"path": ["channel"], "summary": "Show a release channel",
     "args": [{"name": "channel", "kind": "positional", "type": "text", "default": "stable"}],
     "action": {"call": {"class": "live", "body": "{\"op\": \"channel\", \"channel\": {{channel|json}}}"}},
     "output": {"format": "channel"}}
  ]
}
```

`arc update` keeps calling the releases provider itself, because it replaces
`arc` and verifies what it installs.

### 16.4 journal

A page is a head of kind 30078 and parts of kind 3275, all sealed to the
owner. An append is one more part. A KPI record is an event of kind 3276.

```json
{
  "interface": 1, "id": "journal", "shape": "data",
  "title": "Journal", "summary": "A private notebook, sealed to your own key.",
  "kinds": {
    "head": {"kind": 30078, "visibility": "sealed"},
    "part": {"kind": 3275, "visibility": "sealed"},
    "kpi":  {"kind": 3276, "visibility": "sealed"}
  },
  "formats": {
    "page": {"record": "{{text}}"},
    "list": {"record": "{{address}}\t{{title}}\t{{created|date}}", "empty": "no pages"},
    "hits": {"record": "{{address}}\t{{score}}\t{{title}}", "empty": "no results"},
    "kpi":  {"record": "{{created|time}}\t{{key}}\t{{value}}\t{{note}}", "empty": "no records"}
  },
  "commands": [
    {"path": ["write"], "summary": "Replace a page with the standard input",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"},
              {"name": "title", "kind": "option", "type": "text"},
              {"name": "body", "kind": "positional", "type": "stdin"}],
     "action": {"publish": {"kind": "head", "d": "{{address|keyed:page}}",
                            "content": {"json": {"address": "{{address}}", "title": "{{title}}"}},
                            "parts": {"kind": "part", "from": "{{body}}"}}}},
    {"path": ["append"], "summary": "Add text to the end of a page",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"},
              {"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "part", "content": "{{text}}\n",
                            "tags": [["a", "{{address|keyed:page|coord:head}}"]]}}},
    {"path": ["read"], "summary": "Show a page, or a range of its lines",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"},
              {"name": "lines", "kind": "option", "type": "lines"}],
     "action": {"query": {"kinds": ["head"], "authors": "me", "tags": {"d": "{{address|keyed:page}}"}, "limit": 1}},
     "output": {"open": {"parse": "json"}, "join": {"lines": "{{lines}}"}, "format": "page"}},
    {"path": ["tail"], "summary": "Show each text as it is appended",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"}],
     "action": {"watch": {"kinds": ["part"], "authors": "me", "tags": {"a": "{{address|keyed:page|coord:head}}"}}},
     "output": {"open": {"parse": "text"}, "format": "page"}},
    {"path": ["ls"], "summary": "List the pages",
     "args": [{"name": "prefix", "kind": "positional", "type": "text"}],
     "action": {"query": {"kinds": ["head"], "authors": "me"}},
     "output": {"open": {"parse": "json"}, "where": [{"field": "address", "prefix": "{{prefix}}"}],
                "sort": {"field": "address", "order": "asc"}, "format": "list"}},
    {"path": ["search"], "summary": "Search the pages",
     "args": [{"name": "query", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"query": {"kinds": ["head"], "authors": "me"}},
     "output": {"open": {"parse": "json"}, "join": {},
                "rank": {"query": "{{query}}", "fields": ["title", "text"], "limit": 20}, "format": "hits"}},
    {"path": ["kpi", "set"], "summary": "Record one measured value",
     "args": [{"name": "notebook", "kind": "positional", "type": "text", "required": true},
              {"name": "key", "kind": "positional", "type": "text", "required": true},
              {"name": "value", "kind": "positional", "type": "text", "required": true},
              {"name": "note", "kind": "option", "type": "text"}],
     "action": {"publish": {"kind": "kpi", "tags": [["k", "{{notebook|keyed:kpi}}"]],
                            "content": {"json": {"key": "{{key}}", "value": "{{value}}", "note": "{{note}}"}}}}},
    {"path": ["kpi", "latest"], "summary": "Show the last value of each key",
     "args": [{"name": "notebook", "kind": "positional", "type": "text", "required": true}],
     "action": {"query": {"kinds": ["kpi"], "authors": "me", "tags": {"k": "{{notebook|keyed:kpi}}"}}},
     "output": {"open": {"parse": "json"}, "latest": {"by": "key"}, "format": "kpi"}}
  ]
}
```

### 16.5 dm

```json
{
  "interface": 1, "id": "dm", "shape": "data",
  "title": "Direct messages", "summary": "Private messages, as NIP-17 defines.",
  "kinds": {"message": {"kind": 14, "visibility": "private"}},
  "formats": {
    "inbox": {"record": "{{created|time}}  {{author|petname}}\n{{content|indent}}", "empty": "no messages"}
  },
  "commands": [
    {"path": ["send"], "summary": "Send a message",
     "args": [{"name": "to", "kind": "positional", "type": "key", "required": true},
              {"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "message", "to": ["{{to}}"], "content": "{{text}}",
                            "tags": [["p", "{{to}}"]]}}},
    {"path": ["inbox"], "summary": "Show the messages you received",
     "action": {"query": {"kinds": ["message"], "authors": "any"}},
     "output": {"open": {"parse": "text"}, "where": [{"field": "author", "not": "{{me}}"}],
                "sort": {"field": "created", "order": "asc"}, "format": "inbox"}},
    {"path": ["open"], "summary": "Show one conversation",
     "args": [{"name": "peer", "kind": "positional", "type": "key", "required": true}],
     "action": {"query": {"kinds": ["message"], "authors": "any"}},
     "output": {"open": {"parse": "text"},
                "where": [{"any": [{"field": "author", "is": "{{peer}}"}, {"field": "tags.p", "is": "{{peer}}"}]}],
                "sort": {"field": "created", "order": "asc"}, "format": "inbox"}}
  ]
}
```

### 16.6 agora

A board is one announcement. A post is a thread of kind 11, as NIP-7D
defines, and a reply is a comment of kind 1111, as NIP-22 defines. Each names
the board with an `a` tag, and goes to the relays of the board's author.

A reply here answers the post itself, so the post is both the root and the
parent. NIP-22 needs both scopes: `E`, `K` and `P` for the root, and `e`, `k`
and `p` for the parent.

```json
{
  "interface": 1, "id": "agora", "shape": "data",
  "title": "Agora", "summary": "A public board of signed posts and replies.",
  "kinds": {
    "post":  {"kind": 11, "visibility": "public"},
    "reply": {"kind": 1111, "visibility": "public"}
  },
  "formats": {
    "feed": {"record": "{{id|short}}  {{author|petname}}  {{created|time}}\n{{content|truncate:240|indent}}", "empty": "no posts"}
  },
  "commands": [
    {"path": ["post"], "summary": "Publish one post",
     "args": [{"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "post", "content": "{{text}}", "relays": "author",
                            "tags": [["a", "30272:{{author}}:agora"]]}}},
    {"path": ["reply"], "summary": "Reply to a post",
     "args": [{"name": "root", "kind": "positional", "type": "text", "required": true},
              {"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "reply", "content": "{{text}}", "relays": "author",
                            "tags": [["E", "{{root}}", "", "{{root|event_author}}"], ["K", "11"], ["P", "{{root|event_author}}"],
                                     ["e", "{{root}}", "", "{{root|event_author}}"], ["k", "11"], ["p", "{{root|event_author}}"],
                                     ["a", "30272:{{author}}:agora"]]}}},
    {"path": ["feed"], "summary": "Show the newest posts",
     "args": [{"name": "limit", "kind": "option", "type": "integer", "default": "20"}],
     "action": {"query": {"kinds": ["post"], "authors": "any", "tags": {"a": "30272:{{author}}:agora"}, "limit": "{{limit}}"}},
     "output": {"sort": {"field": "created", "order": "desc"}, "format": "feed"}},
    {"path": ["thread"], "summary": "Show the replies to a post",
     "args": [{"name": "root", "kind": "positional", "type": "text", "required": true}],
     "action": {"query": {"kinds": ["reply"], "authors": "any", "tags": {"E": "{{root}}"}}},
     "output": {"thread": {"parent": "tags.e"}, "format": "feed"}}
  ]
}
```

### 16.7 files

A file is a head of kind 3277 and parts of kind 3275, all sealed to the owner.
The head holds the name, the size, and the SHA-256 hash of the file.

```json
{
  "interface": 1, "id": "files", "shape": "data",
  "title": "Files", "summary": "Private files, sealed to your own key.",
  "kinds": {
    "file": {"kind": 3277, "visibility": "sealed"},
    "part": {"kind": 3275, "visibility": "sealed"}
  },
  "formats": {
    "list": {"record": "{{id|short}}\t{{name}}\t{{size}} bytes\t{{created|date}}", "empty": "no files"},
    "put":  {"record": "{{id}}"}
  },
  "commands": [
    {"path": ["put"], "summary": "Store a file",
     "args": [{"name": "file", "kind": "positional", "type": "file", "required": true}],
     "action": {"publish": {"kind": "file",
                            "content": {"json": {"name": "{{file.name}}", "size": "{{file.size}}", "sha256": "{{file.sha256}}"}},
                            "parts": {"kind": "part", "from": "{{file}}", "size": 32768, "max": 256}}},
     "output": {"format": "put"}},
    {"path": ["list"], "summary": "List your files",
     "action": {"query": {"kinds": ["file"], "authors": "me"}},
     "output": {"open": {"parse": "json"}, "sort": {"field": "created", "order": "desc"}, "format": "list"}},
    {"path": ["get"], "summary": "Write one file back to disk",
     "args": [{"name": "id", "kind": "positional", "type": "text", "required": true},
              {"name": "output", "kind": "option", "type": "path", "required": true}],
     "action": {"query": {"kinds": ["file"], "authors": "me", "ids": "{{id}}"}},
     "output": {"open": {"parse": "json"}, "join": {}, "save": {"field": "text", "to": "{{output}}", "sha256": "{{sha256}}"}}}
  ]
}
```

A `file` argument offers `name`, `size`, and `sha256` to templates. `save`
checks the SHA-256 hash of what it writes against the head, and refuses a
mismatch.

## 17. Phases

| Phase | Scope | Proof |
| --- | --- | --- |
| A | The manifest reader, arguments, templates, `call`, and `format`. exec, sqlite and releases in version 1. | `arc exec run echo hello` answers through the installed manifest. |
| B | `publish` and `query` with public and sealed kinds, parts, `open`, `join`, `where`, `sort`, `save`. files and the journal in version 1. | A journal page and a file cross a relay and a USB stick, through manifests only. The `journal` package is gone. |
| C | Private kinds through the mail layer, `watch`, `rank`, `latest`, `thread`. dm and Agora in version 1. | A direct message opens in a NIP-17 client. An Agora post opens in a NIP-7D client. |
| D | Install consent, reserved kinds, `--dry-run`, `--json`. | A manifest that names a reserved kind does not install. A new kind asks the citizen again. |

## 18. Open questions

- **Rollback of sealed data.** A relay can serve an older head of a journal
  page. Version 1 keeps the newest head that the store holds, and does not
  detect an older one that a fresh machine sees first.
- **Deletion.** A data capability cannot delete events, because kind 5 is
  reserved. A later version can add a deletion that names only the
  capability's own kinds.
- **Board moderation.** An Agora board shows every post that names it. A
  board author cannot remove a post yet.
- **Large replies.** A call reply travels as one event. A reply larger than
  the relay limit needs parts, as `publish` has.
