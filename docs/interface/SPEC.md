# Capability interface, version 1

Status: phases A to D are built, see section 18. `arcn` runs them, and
`mise run interface` proves them. This interface replaces the command
line interfaces of the older stack, versions 1 to 4. Those interfaces needed
code in core for direct messages, Agora, and files.

## 1. Purpose

A capability tells `arc` which commands it adds, and what each command does.
The capability says this in its manifest. The manifest names primitives, and
core runs them. The manifest holds no code.

Three rules follow:

- **A new capability needs a manifest, and nothing in core.** The primitives
  are the same for every capability. A journal, a board, and a file store are
  manifests over the same primitives.
- **A manifest cannot make a citizen's key do anything outside the
  primitives.** Core owns every primitive. A provider cannot run code on the
  caller's machine.
- **Where Nostr has a standard, ARC uses it.** A journal page is a NIP-37
  draft of a NIP-23 article, a direct message is NIP-17, and a board is a
  NIP-29 group. A Nostr client that knows these NIPs opens ARC data. ARC
  defines its own kinds only where no NIP fits.

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
| action | What a command does: call, publish, delete, query, or watch. |
| record | One item that the output pipeline works on. |
| pipeline | The output primitives of a command, in their fixed order. |
| kind name | A name that a manifest gives to one event kind, in its `kinds` section. |
| draft | A NIP-37 draft wrap: an event of kind 31234 whose content is another event, sealed to its author. |

## 4. The manifest

```json
{
  "interface": 1,
  "id": "journal",
  "title": "Journal",
  "summary": "A private notebook, sealed to your own key.",
  "shape": "data",
  "kinds": {
    "page": {"kind": 30023, "visibility": "sealed"}
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
| `group` | For a capability with group kinds: the relay that hosts the group, and the group id. See 4.2. |
| `service` | For a service: the default method, path, and body limit. See 4.3. |
| `formats` | Named ways to show records. See section 10. |
| `commands` | The commands. See 4.4. |

### 4.1 Kinds and visibility

Each entry names one event kind and its visibility. The visibility decides the
NIP that carries the event, and the relays that it goes to. A manifest never
names relays.

| Visibility | Carried as | Goes to |
| --- | --- | --- |
| `public` | The event itself, as its NIP defines. | The citizen's NIP-65 write relays. |
| `sealed` | A NIP-37 draft. The event is sealed to the citizen's own key with NIP-44, and the draft carries `["-"]`, the NIP-70 tag, so only the citizen can publish it to a relay. | The relays of the citizen's NIP-37 list, kind 10013. |
| `private` | A rumor inside a NIP-59 gift wrap, through the mail layer. | Each recipient's NIP-17 list, kind 10050, and couriers. |
| `group` | The event, with an `h` tag that names the group, and `["-"]`. | The relay of the group, see 4.2. |

A command can publish only kinds that the manifest names here. Section 12
lists the kinds that no manifest can name.

A sealed kind is the kind of the event inside the draft. A journal page, for
example, is a kind 30023 article inside a kind 31234 draft. The draft's `k`
tag names 30023, as NIP-37 requires.

### 4.2 Group

```json
"group": {"relay": "wss://board.example", "id": "agora"}
```

The group is a NIP-29 group. Its relay enforces who may post, and applies the
moderation of the group's admins. The citizen who runs that relay can
announce a different relay or id in a new version of the manifest.

The operator of the relay makes the group and names its admins:
`arcn relay serve --group agora --admin <key>`. The relay signs the state of
the group with its own key, and names that key in its NIP-11 document as
`self`. It refuses an event for a group that does not exist, a post to a
restricted group from a citizen who is not a member, and a moderation event
from a citizen who is not an admin. `delivery/groups` holds these rules.

### 4.3 Service

```json
"service": {"method": "EXEC", "path": "/", "max_bytes": 1048576}
```

A command of a service can override the method and the path.

### 4.4 Commands

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
| `positional` | In order. The last positional can set `"variadic": true`. Every word after the first word of a variadic argument belongs to it, flags included. |
| `option` | `--name value`. |
| `switch` | `--name`, with no value. |

| Type | Meaning |
| --- | --- |
| `text` | Any text. A variadic text is a list of words. A template shows the list as its words, joined by one space. |
| `integer` | A whole number. |
| `key` | A citizen. Core resolves 64 hex characters, an `npub` or `nprofile` of NIP-19, a NIP-05 name, a petname of this machine, or an installed name, to a public key. |
| `event` | An event. Core resolves 64 hex characters, or a `note`, `nevent` or `naddr` of NIP-19. |
| `file` | A local path that core reads. |
| `path` | A local path that core writes to. It must not exist. |
| `address` | Text that must match the argument's `pattern`. |
| `lines` | A range of lines: `a:b`, `a:`, or `:b`. |
| `stdin` | The standard input. A command has at most one. |

An argument can set `"required": true`, and a `"default"`.

Core shows keys as `npub` and events as `nevent`, as NIP-19 defines. It shows
a citizen's name from their kind 0 profile when it holds one, and the petname
otherwise.

## 6. Templates

A value in an action can hold placeholders: `{{name}}`, where `name` is an
argument. A filter changes the value: `{{name|json}}`. Filters apply from left
to right.

| Filter | Result |
| --- | --- |
| `json` | The value as a JSON literal. An absent value is `null`. |
| `hex` | The value as lower-case hex. |
| `join` | A list as one text: its words, joined by one space. `{{argv\|json}}` writes a JSON array, and `{{sql\|join\|json}}` writes one JSON string. |
| `keyed:<purpose>` | An HMAC of the value, as 22 characters. See 6.1. |
| `event_author` | The author of the event that the value names. Core reads the event from the store, then from the transports, and fails the command when it finds none. |
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
key   = HKDF-SHA256(ikm = secret key, salt = "",
                    info = "arc-keyed-v1" || 0x00 || hex(author) || 0x00 || capability id || 0x00 || purpose,
                    length = 32)
value = base64url(first 16 bytes of HMAC-SHA256(key, input)), without padding
```

A keyed value has 22 characters. A relay indexes a tag value of at most 100
characters, and a checkpoint or a part names its draft by a coordinate of 71
characters and the `d` tag. A `d` tag therefore has at most 29 characters, and
core refuses a longer one.

A placeholder can name several values: `{{notebook+key|keyed:kpi}}` keys the
two values together. Core keys the words of such a list apart, so `a+bc` and
`ab+c` differ.

The key depends on the author and the capability id. One capability therefore
cannot compute the keyed values of another. A citizen who signs through a
NIP-46 remote signer does not hold the secret key: core then asks the signer
for the HMAC, and fails the command when the signer cannot give it.

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

The reply becomes one record. A reply larger than one event travels as parts,
as 7.2.2 defines.

### 7.2 publish

`publish` makes one event, signs it, and sends it where its visibility says.

```json
"publish": {
  "kind": "page",
  "d": "{{address|keyed:page}}",
  "content": "{{body}}",
  "tags": [["title", "{{title}}"]],
  "revise": "replace",
  "to": ["{{recipient}}"]
}
```

| Field | Meaning |
| --- | --- |
| `kind` | A kind name from `kinds`. |
| `d` | For an addressable kind, or for any sealed kind, the `d` tag. For a sealed kind, the draft carries it. |
| `content` | A template for text, or `{"json": ...}` for an object whose values are templates. A `stdin` or `file` argument can supply it. |
| `tags` | Tags, whose values are templates. A tag whose value is empty is left out. |
| `revise` | For a sealed kind: `replace` or `append`. See 7.2.1. |
| `to` | For a private kind: the recipients, as `key` arguments. |

#### 7.2.1 Revisions of a sealed event

A sealed event is a NIP-37 draft, so it can change. Each publish of a draft
also publishes a checkpoint, kind 1234, whose `a` tag names the draft and whose
content is that revision, sealed to the citizen. The checkpoints are the
history of the draft, as NIP-37 defines.

- `replace` makes the new content the whole of the event.
- `append` reads the current draft, adds the new content at its end, and
  publishes the result. It reads from the store first, then from the relays of
  the citizen's NIP-37 list.

#### 7.2.2 Content larger than one event

A relay caps the size of one event. Content of at most 32 KiB travels inside
the event itself, so a Nostr client reads it whole. Longer content travels in
parts:

1. The event holds the first 32 KiB, cut at a line end.
2. Core publishes the rest in parts of at most 32 KiB, as events of kind 3275.
   Each part is sealed to the citizen, carries `["-"]`, and names the event's
   coordinate or ID in its `a` or `e` tag.
3. The event gains a tag `parts` that lists the IDs of the parts in order,
   and a tag `lines` that gives the number of line ends in the content and in
   each part. Core publishes the parts first, and the event last.

A Nostr client that does not know kind 3275 reads the first 32 KiB. A page of
at most 32 KiB is therefore a plain NIP-37 draft. A body holds at most 256
parts.

### 7.3 delete

`delete` asks relays to remove events, as NIP-09 defines. It can name only the
citizen's own events of kinds that the manifest names.

```json
"delete": {"kind": "page", "d": "{{address|keyed:page}}"}
```

For a sealed kind, core publishes the draft with blank content, which NIP-37
reads as deleted. It then publishes a kind 5 request with an `a` tag for the
draft, an `e` tag for the current version of the draft and for each checkpoint
and part, and a `k` tag for each kind. The request is one second older than
the blank draft, so it removes every older version, and the blank draft stays.
A relay and the store therefore drop every version, as NIP-09 defines. The
store also refuses each of these events after the request, so a stick made
before the delete does not bring the page back.

A new version of a draft is always newer than the version before it, by at
least one second, so it replaces that version.

### 7.4 query

`query` reads events from the store, and first asks the transports for any
that the store lacks.

```json
"query": {
  "kinds": ["page"],
  "authors": "me",
  "d": "{{address|keyed:page}}",
  "limit": 1
}
```

| Field | Meaning |
| --- | --- |
| `kinds` | Kind names from `kinds`. |
| `authors` | `me`, `author`, `any`, or a template that names a `key` argument. |
| `d` | The `d` tag, or for a sealed kind, the `d` tag of the draft. |
| `tags` | Other tag filters, whose values are templates. An empty value drops the filter. |
| `ids` | A template that names events. |
| `history` | For a sealed kind: read the checkpoints of the draft, oldest first, instead of the draft. Core first reads the deletion requests of the draft, so the store drops the checkpoints of a deleted draft. |
| `since`, `until`, `limit` | As NIP-01 defines. |

Each value can be a template. A query asks every relay that the visibility
names, and keeps the newest version of each replaceable event, as NIP-01
defines. The store never goes back to an older version that it has seen.

A query of a sealed kind reads the drafts, and filters them by their `k` tag.
A query of a private kind first syncs the mail with each relay, then reads the
messages that this citizen received and sent, from their seals. A query of a
group kind reads the group's relay, and not the store: a post that an admin
removed does not show, and a query fails when the group's relay does not
answer.

### 7.5 watch

`watch` is a query that stays open. It runs for sealed, public and group
kinds. It does not run for private kinds yet. It runs the pipeline on each event that
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

For a sealed kind, the record is the event inside the draft. `open` adds the
fields of parsed content. `join` puts the whole content in `text`. A reply to a
call becomes one record whose `content` is the reply, and whose `error` is set
when the provider refused.

## 9. The output pipeline

A command lists its output primitives. Core runs them in this order, whatever
the order in the manifest:

| Order | Primitive | What it does |
| --- | --- | --- |
| 1 | `open` | Opens drafts and private events, and parses content as `text`, `json`, or `frontmatter` into fields. |
| 2 | `join` | Puts the whole content of each record in `text`, parts included. See 9.1. |
| 3 | `where` | Keeps records whose fields match. |
| 4 | `latest` | Keeps the newest record for each value of a field. |
| 5 | `rank` | Orders records by BM25 against a query, over named fields. |
| 6 | `thread` | Orders records as replies, by a field that names the parent. |
| 7 | `sort`, `limit` | Orders by a field, and cuts. |
| 8 | `tail` | In a watch: shows only what each new version adds. See 9.3. |
| 9 | `save` | Writes a field to a `path` argument, instead of showing it. |
| 10 | `format` | Shows each record with a named format, see section 10. |

```json
"output": {
  "open": {"parse": "text"},
  "join": {"lines": "{{lines}}"},
  "format": "page"
}
```

### 9.1 join

For each record, `join` writes the content inside the event, then the content
of each part that its `parts` tag lists, in order. It fetches the parts from
the store, then from the transports.

If a listed part is missing, `join` stops after the text before it, and
reports the missing part. With `lines`, `join` reads the `lines` tag, and
fetches only the parts that hold those lines. When `join` is the last primitive before `format`, it writes each
part as it arrives.

### 9.2 where, latest, rank, thread, sort

```json
"where":  [{"field": "tags.t", "lacks": "deleted"}, {"field": "address", "prefix": "{{path}}"}],
"latest": {"by": "key"},
"rank":   {"query": "{{query}}", "fields": ["title", "text"], "limit": 20},
"thread": {"parent": "tags.e"},
"sort":   {"field": "created", "order": "asc"}
```

A condition whose value template is empty is dropped, so an optional argument
can narrow a query or leave it whole. `any` holds conditions of which one must
match.

`rank` uses BM25 with `k1 = 1.2` and `b = 0.75`, over lower-case runs of
Unicode letters and digits. It adds a field `score`.

### 9.3 tail

In a watch, `tail` compares each new version of a record with the version
that it showed before. When the new text starts with the old text, `tail`
shows only what follows it. Otherwise it shows a line that says the record was
rewritten, and then the whole new text.

## 10. Formats

A format renders records as text:

```json
"formats": {
  "page": {"record": "{{text}}"},
  "list": {"record": "{{address}}\t{{title}}\t{{created|date}}", "empty": "no pages"},
  "rows": {"table": {"columns": "results.0.columns", "rows": "results.0.rows"}}
}
```

| Field | Meaning |
| --- | --- |
| `header` | A template shown once, before the records. |
| `record` | A template shown for each record. |
| `empty` | Shown when there is no record. |
| `table` | Shows a record's rows as aligned columns, instead of `record`. |

A template or a table names a field of a record by its path: `tags.d`, or
`results.0.rows`. A number in a path takes that item of a list, from 0.

Format templates have these helpers: `time`, `date`, `name`, `npub`, `nevent`,
`short`, `indent`, `truncate:n`, and `default:<text>`. `name` shows a citizen's
profile name, or their petname.

Core removes every control character except newline and tab from each value
before it shows it. A value from an event cannot move the cursor, or change
the terminal.

## 11. Flags of every command

| Flag | Meaning |
| --- | --- |
| `--json` | Writes each record as one JSON object on one line, after the pipeline and before `format`. Agents read this. |
| `--dry-run` | For `publish` and `delete`: writes each event that core would sign, and signs nothing. |
| `--later` | For a live `call`: waits in the outbox instead. |

## 12. Security

**Install shows what a capability can do.** Before a citizen trusts a
capability, `arc install` shows its author, its shape, each kind that it
publishes with its visibility, and its group relay if it has one. A capability
that publishes a public or group kind can post under the citizen's name, and
the install says so.

**Some kinds are reserved.** No manifest can name these kinds. They speak for
the citizen's identity, or core makes them itself:

| Kind | Why |
| --- | --- |
| 0, 3 | Profile and follows. |
| 5 | Deletion. Core makes it, for `delete` only. |
| 13, 1059, 21059 | Seals and gift wraps. The mail layer makes them. |
| 62 | Request to vanish. |
| 1234, 31234, 3275 | Checkpoints, drafts, and parts. Core makes them for sealed kinds. |
| 3272, 3273, 3274 | Calls and acknowledgements. Core makes them. |
| 9734, 9735 | Zaps. |
| 10002, 10013, 10050 | Relay lists. |
| 10272, 30272 | Migration records and announcements. |
| 39000 to 39009 | The state of a NIP-29 group, which only its relay signs. |
| 13194, 23194, 23195 | Wallet connect. |
| 22242, 24133, 27235 | Authentication and remote signing. |

The kinds 9000 to 9020, which NIP-29 defines for moderation, can have only the
visibility `group`. The group's relay decides whether the citizen may act.

**Recipients come from the citizen.** A private event goes only to keys that
the citizen typed as `key` arguments, and to the citizen's own seal. No value
from an event or a reply can add a recipient.

**Files come from the citizen.** Core reads a file only when the citizen typed
its path as a `file` argument. Core writes a file only to a `path` argument
that does not exist, and never replaces a file.

**Sealed data stays with its author.** Every draft, checkpoint and part
carries the NIP-70 tag, so a relay accepts it only from its author after NIP-42
authentication. Nobody else can publish an old version of it again.

**Keyed values stay inside a capability.** See 6.1.

**A reply is data.** Core never runs, opens, or follows what a reply or an
event holds.

## 13. Keys

A citizen's key comes from one of these, as NIP-19, NIP-49 and NIP-46 define:

| Source | Meaning |
| --- | --- |
| `nsec` | The secret key, in a file that only its owner can read. |
| `ncryptsec` | The secret key, encrypted with a passphrase. Core asks for the passphrase. |
| `bunker://` or a NIP-05 name | A NIP-46 remote signer. Core never holds the secret key. An agent signs through a signer that its owner controls. |

`arcn key new --encrypt` and `arcn key encrypt` seal a key with a passphrase.
Core reads the passphrase from `ARCN_PASSPHRASE`, or asks on the terminal.

`arcn key bunker --relay <url>` serves this citizen's key as a NIP-46 signer,
and prints its `bunker://` URI. With `--allow-kind`, it signs only those kinds,
and NIP-42 authentication for relays. `arcn key use <uri>` makes a home that
signs through it. That home has no mail, no calls, and no keyed values, because
each of them needs the secret key on the machine.

## 14. Install and dispatch

`arc install <author> [capability]` reads the announcement, verifies it, shows
what section 12 requires, and asks the citizen once. The capability's name is
its id, unless the citizen gives another with `--as`.

`arc <name> <path...> [args]` finds the installed capability, then the command
with the longest matching path, and runs it. `arc help <name>` lists the
commands.

A new version of a manifest replaces the old one when its author announces
it. If the new version publishes a kind that the old one did not, makes a kind
more visible, or names another group relay, core stops each command of it,
and says what changed, until the citizen installs it again. Core fetches the
author's newest announcement before each command, so it sees a new version at
once. Visibility grows in this order: sealed, private, group, public.

## 15. Versions

This interface is version 1. Core refuses a manifest of a later version, and
says which version it would need. A new primitive, or a new field with a new
meaning, comes in a new version. The older stack's interfaces, versions 1 to
4, do not run on the delivery layer.

## 16. What leaves core

| Today | In version 1 |
| --- | --- |
| The `journal` package | The journal manifest: NIP-37 drafts of NIP-23 articles. |
| The direct-message renderers and cache in `toolbox` | The dm manifest: NIP-17. |
| The `agora` input source | The agora manifest: a NIP-29 group, with NIP-7D threads and NIP-22 comments. |
| The `private_file` and `sealed_file` inputs | The files manifest: NIP-37 drafts of NIP-94 file metadata. |
| The `seal`, `pubkey` and `shell` filters | `key` arguments, private kinds, and `json`. |

## 17. The capabilities in version 1

### 17.1 exec

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
     "action": {"call": {"class": "later", "body": "{\"action\": \"start\", \"script\": {{script|join|json}}}"}},
     "output": {"open": {"parse": "json"}, "format": "job"}},
    {"path": ["status"], "summary": "Show a job",
     "args": [{"name": "job", "kind": "positional", "type": "text", "required": true}],
     "action": {"call": {"class": "live", "body": "{\"action\": \"status\", \"job\": {{job|json}}}"}},
     "output": {"open": {"parse": "json"}, "format": "run"}}
  ]
}
```

### 17.2 sqlite

```json
{
  "interface": 1, "id": "sqlite", "shape": "service",
  "title": "SQLite", "summary": "Answers SQL for the citizens that it grants.",
  "service": {"method": "QUERY", "path": "/main", "max_bytes": 1048576},
  "kinds": {},
  "formats": {"rows": {"table": {"columns": "results.0.columns", "rows": "results.0.rows"}}},
  "commands": [
    {"path": [], "summary": "Run SQL",
     "args": [{"name": "sql", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"call": {"class": "live", "body": "{\"sql\": {{sql|join|json}}}"}},
     "output": {"open": {"parse": "json"}, "format": "rows"}}
  ]
}
```

### 17.3 releases

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

### 17.4 journal

A page is a NIP-23 article, kind 30023, inside a NIP-37 draft. The article's
`d` tag is the page address, sealed inside the draft; the draft's `d` tag is a
keyed value of the address. A client that knows NIP-37 opens the page as a
draft article. The checkpoints of the draft are the history of the page.

A KPI series is one draft per notebook and key, around an event of kind 30078,
which NIP-78 defines for the data of one application. The draft holds the
latest value, and its checkpoints hold every value before it.

```json
{
  "interface": 1, "id": "journal", "shape": "data",
  "title": "Journal", "summary": "A private notebook, sealed to your own key.",
  "kinds": {
    "page": {"kind": 30023, "visibility": "sealed"},
    "kpi":  {"kind": 30078, "visibility": "sealed"}
  },
  "formats": {
    "page":    {"record": "{{text}}"},
    "list":    {"record": "{{tags.d}}\t{{tags.title}}\t{{created|date}}", "empty": "no pages"},
    "hits":    {"record": "{{tags.d}}\t{{score}}\t{{tags.title}}", "empty": "no results"},
    "history": {"record": "{{created|time}}\n{{text|truncate:200|indent}}", "empty": "no revisions"},
    "kpi":     {"record": "{{created|time}}\t{{key}}\t{{value}}\t{{note}}", "empty": "no records"}
  },
  "commands": [
    {"path": ["write"], "summary": "Replace a page with the standard input",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"},
              {"name": "title", "kind": "option", "type": "text"},
              {"name": "body", "kind": "positional", "type": "stdin"}],
     "action": {"publish": {"kind": "page", "d": "{{address|keyed:page}}", "revise": "replace", "content": "{{body}}",
                            "tags": [["d", "{{address}}"], ["title", "{{title}}"]]}}},
    {"path": ["append"], "summary": "Add text to the end of a page",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"},
              {"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "page", "d": "{{address|keyed:page}}", "revise": "append", "content": "{{text}}\n",
                            "tags": [["d", "{{address}}"]]}}},
    {"path": ["read"], "summary": "Show a page, or a range of its lines",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"},
              {"name": "lines", "kind": "option", "type": "lines"}],
     "action": {"query": {"kinds": ["page"], "authors": "me", "d": "{{address|keyed:page}}", "limit": 1}},
     "output": {"open": {"parse": "text"}, "join": {"lines": "{{lines}}"}, "format": "page"}},
    {"path": ["tail"], "summary": "Show each text as it is appended",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"}],
     "action": {"watch": {"kinds": ["page"], "authors": "me", "d": "{{address|keyed:page}}"}},
     "output": {"open": {"parse": "text"}, "join": {}, "tail": {}, "format": "page"}},
    {"path": ["history"], "summary": "List the revisions of a page",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"}],
     "action": {"query": {"kinds": ["page"], "authors": "me", "d": "{{address|keyed:page}}", "history": true}},
     "output": {"open": {"parse": "text"}, "join": {}, "format": "history"}},
    {"path": ["ls"], "summary": "List the pages",
     "args": [{"name": "prefix", "kind": "positional", "type": "text"}],
     "action": {"query": {"kinds": ["page"], "authors": "me"}},
     "output": {"open": {"parse": "text"}, "where": [{"field": "tags.d", "prefix": "{{prefix}}"}],
                "sort": {"field": "tags.d", "order": "asc"}, "format": "list"}},
    {"path": ["search"], "summary": "Search the pages",
     "args": [{"name": "query", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"query": {"kinds": ["page"], "authors": "me"}},
     "output": {"open": {"parse": "text"}, "join": {},
                "rank": {"query": "{{query}}", "fields": ["tags.title", "text"], "limit": 20}, "format": "hits"}},
    {"path": ["delete"], "summary": "Delete a page and its history",
     "args": [{"name": "address", "kind": "positional", "type": "address", "required": true,
               "pattern": "^[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*){2}$"}],
     "action": {"delete": {"kind": "page", "d": "{{address|keyed:page}}"}}},
    {"path": ["kpi", "set"], "summary": "Record one measured value",
     "args": [{"name": "notebook", "kind": "positional", "type": "text", "required": true},
              {"name": "key", "kind": "positional", "type": "text", "required": true},
              {"name": "value", "kind": "positional", "type": "text", "required": true},
              {"name": "note", "kind": "option", "type": "text"}],
     "action": {"publish": {"kind": "kpi", "d": "{{notebook+key|keyed:kpi}}", "revise": "replace",
                            "tags": [["d", "{{key}}"], ["notebook", "{{notebook}}"]],
                            "content": {"json": {"key": "{{key}}", "value": "{{value}}", "note": "{{note}}"}}}}},
    {"path": ["kpi", "latest"], "summary": "Show the last value of each key",
     "args": [{"name": "notebook", "kind": "positional", "type": "text", "required": true}],
     "action": {"query": {"kinds": ["kpi"], "authors": "me"}},
     "output": {"open": {"parse": "json"}, "where": [{"field": "tags.notebook", "is": "{{notebook}}"}], "format": "kpi"}},
    {"path": ["kpi", "log"], "summary": "Show every value of one key",
     "args": [{"name": "notebook", "kind": "positional", "type": "text", "required": true},
              {"name": "key", "kind": "positional", "type": "text", "required": true}],
     "action": {"query": {"kinds": ["kpi"], "authors": "me", "d": "{{notebook+key|keyed:kpi}}", "history": true}},
     "output": {"open": {"parse": "json"}, "format": "kpi"}}
  ]
}
```

### 17.5 dm

```json
{
  "interface": 1, "id": "dm", "shape": "data",
  "title": "Direct messages", "summary": "Private messages, as NIP-17 defines.",
  "kinds": {"message": {"kind": 14, "visibility": "private"}},
  "formats": {
    "inbox": {"record": "{{created|time}}  {{author|name}}\n{{content|indent}}", "empty": "no messages"}
  },
  "commands": [
    {"path": ["send"], "summary": "Send a message",
     "args": [{"name": "to", "kind": "positional", "type": "key", "required": true},
              {"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "message", "to": ["{{to}}"], "content": "{{text}}", "tags": [["p", "{{to}}"]]}}},
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

### 17.6 agora

A board is a NIP-29 group. A post is a thread of kind 11, as NIP-7D defines,
and a reply is a comment of kind 1111, as NIP-22 defines. The group's relay
decides who may post, and its admins moderate. Any client that knows NIP-29
opens the board.

A reply here answers the post itself, so the post is both the root and the
parent. NIP-22 needs both scopes: `E`, `K` and `P` for the root, and `e`, `k`
and `p` for the parent.

```json
{
  "interface": 1, "id": "agora", "shape": "data",
  "title": "Agora", "summary": "A public board of signed posts and replies.",
  "group": {"relay": "wss://board.example", "id": "agora"},
  "kinds": {
    "post":   {"kind": 11, "visibility": "group"},
    "reply":  {"kind": 1111, "visibility": "group"},
    "remove": {"kind": 9005, "visibility": "group"}
  },
  "formats": {
    "feed": {"record": "{{id|nevent}}  {{author|name}}  {{created|time}}\n{{tags.title}}\n{{content|truncate:240|indent}}", "empty": "no posts"}
  },
  "commands": [
    {"path": ["post"], "summary": "Publish one post",
     "args": [{"name": "title", "kind": "option", "type": "text"},
              {"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "post", "content": "{{text}}", "tags": [["title", "{{title}}"]]}}},
    {"path": ["reply"], "summary": "Reply to a post",
     "args": [{"name": "root", "kind": "positional", "type": "event", "required": true},
              {"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"publish": {"kind": "reply", "content": "{{text}}",
                            "tags": [["E", "{{root}}", "", "{{root|event_author}}"], ["K", "11"], ["P", "{{root|event_author}}"],
                                     ["e", "{{root}}", "", "{{root|event_author}}"], ["k", "11"], ["p", "{{root|event_author}}"]]}}},
    {"path": ["feed"], "summary": "Show the newest posts",
     "args": [{"name": "limit", "kind": "option", "type": "integer", "default": "20"}],
     "action": {"query": {"kinds": ["post"], "authors": "any", "limit": "{{limit}}"}},
     "output": {"sort": {"field": "created", "order": "desc"}, "format": "feed"}},
    {"path": ["thread"], "summary": "Show the replies to a post",
     "args": [{"name": "root", "kind": "positional", "type": "event", "required": true}],
     "action": {"query": {"kinds": ["reply"], "authors": "any", "tags": {"E": "{{root}}"}}},
     "output": {"thread": {"parent": "tags.e"}, "format": "feed"}},
    {"path": ["remove"], "summary": "Remove a post from the board, if you are its admin",
     "args": [{"name": "id", "kind": "positional", "type": "event", "required": true}],
     "action": {"publish": {"kind": "remove", "tags": [["e", "{{id}}"]]}}}
  ]
}
```

### 17.7 files

A file is a NIP-94 file metadata event, kind 1063, inside a NIP-37 draft. The
metadata holds the name, the media type, the size, and the SHA-256 hash, in
the tags that NIP-94 defines. The bytes travel as the content, and in parts
past 32 KiB, as 7.2.2 defines.

```json
{
  "interface": 1, "id": "files", "shape": "data",
  "title": "Files", "summary": "Private files, sealed to your own key.",
  "kinds": {"file": {"kind": 1063, "visibility": "sealed"}},
  "formats": {
    "list": {"record": "{{tags.d}}\t{{tags.alt}}\t{{tags.size}} bytes\t{{created|date}}", "empty": "no files"},
    "put":  {"record": "{{tags.d}}"}
  },
  "commands": [
    {"path": ["put"], "summary": "Store a file",
     "args": [{"name": "file", "kind": "positional", "type": "file", "required": true}],
     "action": {"publish": {"kind": "file", "d": "{{file.sha256|keyed:file}}", "revise": "replace", "content": "{{file}}",
                            "tags": [["d", "{{file.sha256}}"], ["alt", "{{file.name}}"], ["m", "{{file.type}}"],
                                     ["x", "{{file.sha256}}"], ["size", "{{file.size}}"]]}},
     "output": {"format": "put"}},
    {"path": ["list"], "summary": "List your files",
     "action": {"query": {"kinds": ["file"], "authors": "me"}},
     "output": {"open": {"parse": "text"}, "sort": {"field": "created", "order": "desc"}, "format": "list"}},
    {"path": ["get"], "summary": "Write one file back to disk",
     "args": [{"name": "id", "kind": "positional", "type": "text", "required": true},
              {"name": "output", "kind": "option", "type": "path", "required": true}],
     "action": {"query": {"kinds": ["file"], "authors": "me", "d": "{{id|keyed:file}}", "limit": 1}},
     "output": {"open": {"parse": "text"}, "join": {}, "save": {"field": "text", "to": "{{output}}", "decode": "base64", "sha256": "{{tags.x}}"}}},
    {"path": ["delete"], "summary": "Delete a file",
     "args": [{"name": "id", "kind": "positional", "type": "text", "required": true}],
     "action": {"delete": {"kind": "file", "d": "{{id|keyed:file}}"}}}
  ]
}
```

A `file` argument offers `name`, `type`, `size`, and `sha256` to templates.
Its bytes go into the content as base64. `save` decodes them, checks the
SHA-256 hash of what it writes against the `x` tag, and refuses a mismatch.

## 18. Phases

| Phase | Scope | Proof |
| --- | --- | --- |
| A (built) | The manifest reader, arguments, templates, `call`, and `format`. NIP-19 keys and events. exec, sqlite and releases in version 1. | `arc exec run echo hello` answers through the installed manifest. |
| B (built) | Sealed kinds: NIP-37 drafts, checkpoints, NIP-70, the NIP-37 relay list, parts, `delete`. `open`, `join`, `where`, `sort`, `tail`, `save`. The journal and files in version 1. | A journal page crosses a relay and a USB stick. A page of at most 32 KiB opens in a NIP-37 client as a draft article. The `journal` package is gone. |
| C (built) | Private kinds through the mail layer, `watch`, `rank`, `latest`, `thread`. Group kinds, and a relay that enforces NIP-29 on khatru. dm and Agora in version 1. | A direct message opens in a NIP-17 client. An Agora post opens in a NIP-29 client, and an admin removes it. |
| D (built) | Install consent, reserved kinds, `--dry-run`, `--json`, and keys from `ncryptsec` and NIP-46 signers. | A manifest that names a reserved kind does not install. A new kind asks the citizen again. An agent signs through a remote signer. |

## 19. Limits of this design

- **Parts are ARC's own.** No NIP carries private content larger than one
  event. A Nostr client reads the first 32 KiB of a longer page, and no more.
- **NIP-37 is a draft.** Like NIP-17, it can still change. ARC follows its
  text as of this document.
- **The `k` tag of a draft is public.** A relay learns that a draft holds an
  article, a KPI series, or a file, but not its content or its address.
- **Rollback on a fresh machine.** A query asks every relay of the citizen's
  NIP-37 list, and keeps the newest version. If every relay serves an old
  version, a machine that never saw the newer one cannot tell.
- **A remote signer decrypts for its client.** `--allow-kind` limits what the
  bunker signs, and not what it decrypts.
- **A private event goes to one recipient.** NIP-17 allows a message to
  several, and this arc refuses it.
- **The group relay is a subset of NIP-29.** It hosts open and restricted
  groups, admins, removal, and join and leave requests. It does not hide the
  posts of a private group from readers, and has no invite codes or roles
  other than admin.
- **One process per home.** The store is one file that one process opens at
  a time. A `tail` holds it, so a second command on the same home waits.
- **Each read asks the relays.** A query fetches from every relay before it
  reads the store, so a slow relay makes every read slow.
- **Keyed values need the secret key, or a signer that computes them.** A NIP-46
  signer that does not offer this cannot run a capability that uses `keyed`.
