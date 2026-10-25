# App interface, version 1

- Status: built
- Layers: core, sdk, runtime, app
- Owns: core/draft, core/wire, sdk/provider
- Proof: go test -count=1 -run '^(TestInterface|TestDevicePages|TestStorageLargeFile|TestAFileIsOneBody|TestAFileInPartsStillReads|TestABodyOpensOnlyWithTheKeyOfItsApp|TestAValueCannotReachTheTerminal|TestAValueCannotHideText|TestHelpIsEscaped|TestDescribeIsEscaped|TestAReplyThatDoesNotFitCannotReachTheTerminal|TestTheTextOfAFormatCannotReachTheTerminal|TestACleanValueGetsNoNote|TestTheErrorOfAMemberCannotReachTheTerminal|TestAnOlderHomeStillWorks|TestANewHomeIsDotArc|TestDotArcWinsOverTheOlderHome|TestARelayKeepsItsOlderHome|TestTheFlagAndTheVariableWin|TestNoHomeFromTheWorkingDirectory|TestTwoHomesWithAnIdentityWarn|TestAnUnreadableDotArcStopsArc|TestAnUnreadableOlderHomeStopsArc|TestAnUnreadableDefaultIsNoMissingIdentity|TestAnUnreadableDefaultStopsKeysGen|TestTwoDevicesThatAppendOfflineKeepBothEntries|TestTwoOfflineNewsKeepTheirOwnPages|TestABurstKeepsTimeOrderWithTheOtherDevice|TestADeleteAndAnOfflineAppendEndClear|TestALostPageLivesOnItsOwn|TestAPageNumberTooLargeIsRefused|TestADeleteOfTheLivePageKeepsTheLostPage|TestFixTakesTheAddressesOfHistory|TestAHeadFixStaysWhenTheLostPageMovesUp|TestTwoDevicesThatAddOfflineKeepBothTexts|TestEntriesOfOneSecondKeepTheirOrder|TestNewWritesTheNextPage|TestAddWritesToTodaysPage|TestAMissingPageAndTheReservedWords|TestAFixReplacesAnEntryAndTheHistoryKeepsBoth|TestADeleteTakesEachEntry|TestLsOrdersByTheNewestEntry|TestAnAppendIsAnEntry|TestAWriteFillsInTheFrontMatter|TestAnAppCommandDoesNotWaitForATerminal)$' ./internal/proof/ ./runtime/iface/ ./core/draft/ ./cmd/arc/
- Unverified: No test runs a manifest command against a relay other than a local relay. No test opens a page in another Nostr client: no other client holds the content key. No test runs the import of old pages on a store of 1,000 pages. No test opens a body in another Nostr client: NIP-37 has no body. No test runs the Docker image with a volume at the older home `/home/arc/.config/arc`. No test runs arc with a home that root owns: a folder of mode 000 stands in for it. No test syncs entries between two devices through a real storage citizen: the entry tests copy the events between two stores. No test runs `arc sync --repair` on a lost page that moved up; the rule of section 4.7 of the storage spec keeps it by reasoning. An older arc that deletes the page at an address deletes the head of a lost page there too. No test measures `read` on a notebook of 10,000 entries, which opens each entry.

Phases A to F are built, see section 8. `arc` runs them, and
`mise run interface` proves them. This interface replaces the command
line interfaces of the older stack, versions 1 to 4. Those interfaces needed
code in core for direct messages, Agora, and files.



## 1. Purpose

An app tells `arc` which commands it adds, and what each command does. Its
interface manifest selects bounded application-runtime primitives. Those
primitives use core ARC for signed events, delivery, calls and sessions. The
manifest holds no executable code. Capability remains a protocol term for the
interface that a participant offers. See [the app model](../../apps/README.md).

Three rules follow:

- **A new capability needs a manifest, and nothing in core.** The primitives
  are the same for every capability. A journal, a board, and a file store are
  manifests over the same primitives.
- **A manifest cannot make a citizen's key do anything outside the
  primitives.** The application runtime implements these primitives over core
  protocols. A service cannot run arbitrary manifest code on the
  caller's machine.
- **Where Nostr has a standard, ARC uses it.** A journal page has the shape
  of a NIP-37 draft of a NIP-23 article, a direct message is NIP-17, and a group kind
  goes to a NIP-29 group. A Nostr client that knows these NIPs opens ARC data. ARC
  defines its own kinds only where no NIP fits.

This interface runs on the delivery layer of docs/delivery/SPEC.md. Every
datum is a signed event, so signing and verification are not primitives: the
delivery layer does both for every event.

## 2. Terms

| Term | Meaning |
| --- | --- |
| manifest | The JSON document that defines a capability. |
| author | The citizen who signs the announcement of a manifest. For a service, this is the provider. |
| command | One entry that the manifest adds to `arc`. |
| action | What a command does: call, publish, delete, query, or watch. |
| record | One item that the output pipeline works on. |
| pipeline | The output primitives of a command, in their fixed order. |
| kind name | A name that a manifest gives to one event kind, in its `kinds` section. |
| draft | A NIP-37 draft wrap: an event of kind 31234 whose content is another event, sealed with the content key of its capability (section 3.9.4). A draft of the format before the data roots is sealed to its author. |

### 2.1 Two shapes

| Shape | What answers | Example |
| --- | --- | --- |
| service | An app service program answers calls. | exec, sqlite, releases |
| data | Nobody answers. The citizen writes events and reads them. | journal, Agora, files |

A data app needs no service program. Its manifest still comes from an author,
who signs its announcement. A citizen installs the manifest by trusting that
author.

## 3. Rules

### 3.1 The manifest

```json
{
  "interface": 1,
  "id": "journal",
  "title": "Journal",
  "summary": "A private notebook, sealed to your own key.",
  "shape": "data",
  "kinds": {
    "page": {"kind": 30023, "visibility": "sealed", "frontmatter": ["title", "page", "notebook"]}
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
| `kinds` | Every kind that the commands publish or read, by name. See section 3.2. |
| `group` | For a capability with group kinds: the relay that hosts the group, and the group id. See section 3.3. |
| `service` | For a service: the default method, path, and body limit. See section 3.4. |
| `formats` | Named ways to show records. See section 3.15. |
| `commands` | The commands. See section 3.5. |

### 3.2 Kinds and visibility

Each entry names one event kind and its visibility. The visibility decides the
NIP that carries the event, and the relays that it goes to. A manifest never
names relays.

| Visibility | Carried as | Goes to |
| --- | --- | --- |
| `public` | The event itself, as its NIP defines. | The citizen's NIP-65 write relays. |
| `sealed` | A draft in the shape of NIP-37. The event is sealed with NIP-44 and the content key of the capability, and the device key signs the draft, as section 3.9.4 defines. The draft carries `["-"]`, the NIP-70 tag. | The local store only. `arc sync` gives it to the storage citizens of the citizen, as encrypted blobs, see [the storage spec](../storage/SPEC.md). It never goes to a relay. |
| `private` | A rumor inside a NIP-59 gift wrap, through the mail layer. | Each recipient's NIP-17 list, kind 10050, and couriers. |
| `group` | The event, with an `h` tag that names the group, and `["-"]`. | The relay of the group, see section 3.3. |

A kind can declare `"frontmatter": ["title"]`. This is a write policy:
new content and the resulting content of an append must start with a header
that contains each named field with a nonempty string value. Validation runs
before signing, storage or delivery, including a dry run. An existing page
without a header remains readable; rewrite it with a header before appending.
For a kind with `notebook_entry`, an append adds an entry and leaves the
head as it was, so the policy applies to `write`, which fills in the
fields that the text does not give (section 3.9.3).

The header starts and ends with a line of `---`, uses at most 4096 bytes, and
contains flat `key: value` fields. Keys use lowercase letters, digits and
underscores, starting with a letter. Duplicate keys, nested values, arrays,
multiline strings and malformed delimiters are refused. Values are plain
strings, JSON double-quoted strings, or single-quoted strings (`''` escapes a
quote). LF and CRLF delimiter lines are accepted. This format does not implement
full YAML. Optional fields are preserved; only declared fields are required.
If the header has `title`, it supplies the event's title tag. A different
explicit title is refused. Reads preserve the full document when parsed as
`text`; the `frontmatter` output parser separates the fields and body.

A sealed kind can name `notebook_index`, a separate sealed kind in the same
manifest. Notebook pages require title/page/notebook front matter, positive page
numbers, managed timestamps, and automatic index refresh. Notebook application
behavior stays outside core; all events use its existing draft protocol.

A notebook kind can also name `notebook_entry`, a separate sealed kind in
the same manifest. It MUST have no `frontmatter`, no `notebook_index` and
no `notebook_entry` of its own. The pages of the notebook kind then keep
each entry, as section 3.9.3 defines.

A command can publish only kinds that the manifest names here. Section 6
lists the kinds that no manifest can name.

A sealed kind is the kind of the event inside the draft. A journal page, for
example, is a kind 30023 article inside a kind 31234 draft. The draft's `k`
tag names 30023, as NIP-37 requires.

### 3.3 Group

```json
"group": {"relay": "wss://board.example", "id": "board"}
```

The group is a NIP-29 group. Its relay enforces who may post, and applies the
moderation of the group's admins. The author of the manifest can announce a
different relay or id in a new version of the manifest.

`arc relay serve` does not host groups. The relay of a group is a NIP-29
relay outside ARC. The ARC relay only coordinates: it does not keep the data
of an app.

### 3.4 Service

```json
"service": {"method": "EXEC", "path": "/", "max_bytes": 1048576}
```

A command of a service can override the method and the path.

`output` is optional. It is an output pipeline, as section 3.14 defines, for the
reply of a call by address, `arc call`, see section 3.18. It starts from one record
whose `content` is the reply. It can open, filter, format, and set the exit
status. It cannot `save` or `tail`, and its templates name no arguments.
Without `output`, `arc call` writes the reply as it came.

```json
"service": {"method": "QUERY", "path": "/main", "max_bytes": 1048576,
              "interactions": ["request_reply", "server_stream", "duplex"],
            "output": {"open": {"parse": "json"}, "format": "rows"}}
```

A manifest with `output` in its service needs a caller of v0.14.0 or later.
An older caller refuses the manifest, because it does not know the field.

### 3.5 Commands

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

### 3.6 Arguments

| Kind | Meaning |
| --- | --- |
| `positional` | In order. The last positional can set `"variadic": true`. Every word after the first word of a variadic argument belongs to it, flags included. |
| `option` | `--name value`. |
| `switch` | `--name`, with no value. |

| Type | Meaning |
| --- | --- |
| `text` | Any text. A variadic text is a list of words. A template shows the list as its words, joined by one space. |
| `integer` | A whole number. |
| `key` | A citizen. Core resolves 64 hex characters, an `npub` or `nprofile` of NIP-19, a NIP-05 name, a petname of this machine, or an installed name, to a public key. If the text names a list of this command, `arc lists`, core runs the command once for each member. One command takes one list. If a member fails, core runs the other members, and then reports the failures. |
| `event` | An event. Core resolves 64 hex characters, or a `note`, `nevent` or `naddr` of NIP-19. |
| `file` | A local path that core reads. Core reads at most 6 MiB, or 63 MiB for the file of a `body` (section 3.9.5). |
| `path` | A local path that core writes to. It must not exist. |
| `address` | Text that must match the argument's `pattern`. |
| `lines` | A range of lines: `a:b`, `a:`, or `:b`. |
| `stdin` | The standard input. A command has at most one. |

An argument can set `"required": true`, and a `"default"`.

Core shows keys as `npub` and events as `nevent`, as NIP-19 defines. It shows
a citizen's name from their kind 0 profile when it holds one, and the petname
otherwise.

### 3.7 Templates

A value in an action can hold placeholders: `{{name}}`, where `name` is an
argument. A filter changes the value: `{{name|json}}`. Filters apply from left
to right.

| Filter | Result |
| --- | --- |
| `json` | The value as a JSON literal. An absent value is `null`. |
| `hex` | The value as lower-case hex. |
| `join` | A list as one text: its words, joined by one space. `{{argv\|json}}` writes a JSON array, and `{{sql\|join\|json}}` writes one JSON string. |
| `keyed:<purpose>` | An HMAC of the value, as 22 characters. See section 3.7.1. |
| `event_author` | The author of the event that the value names. Core reads the event from the store, then from the transports, and fails the command when it finds none. |
| `default:<text>` | The text, when the value is absent. |

Each template also knows these names:

| Name | Value |
| --- | --- |
| `me` | The citizen's public key. |
| `author` | The public key of the capability's author. |
| `now` | The time now, as seconds since 1970. |

#### 3.7.1 Keyed values

A keyed value names something without revealing it. A journal page, for
example, is found by a keyed value of its address:

```text
root  = HKDF-SHA256(ikm = secret key, salt = "", info = "arc-keyed-root-v1", length = 32)
key   = HKDF-SHA256(ikm = root, salt = "",
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
cannot compute the keyed values of another.

A machine that signs through a NIP-46 remote signer does not hold the secret
key, and cannot derive the root. A machine with the key therefore seals the
root to the citizen's own key, as a NIP-37 draft, when it first computes a
keyed value or starts a bunker. A citizen who never uses keyed values leaves
no such draft. The draft's `d` tag is
`arc-keyed-root`, and its event inside has kind 30078 and the root as hex.
The machine that signs remotely fetches that draft, and asks the signer to
open it. The root follows from the key, so every machine with the key makes
the same draft. A `d` tag of a manifest cannot start with `arc-`, and a query
leaves out these drafts.

### 3.8 The action call

Only a service has `call`. It sends one request to the provider, as
docs/delivery/SPEC.md section 4.10 defines.

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
as section 3.9.2 defines.

### 3.9 The action publish

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
| `revise` | For a sealed kind: `replace` or `append`. See section 3.9.1. |
| `to` | For a private kind: the recipients, as `key` arguments. |
| `body` | For a sealed kind: the one file argument whose raw bytes go beside the draft, as `{{file}}`. See section 3.9.5. |
| `frontmatter_address` | An optional address template. It requires `page` and `notebook` in the kind's front-matter policy, and their combined `notebook/page` must equal the rendered address. |

#### 3.9.1 Revisions of a sealed event

A sealed event is a NIP-37 draft, so it can change. Each publish of a draft
also publishes a checkpoint, kind 1234, whose `a` tag names the draft and whose
content is that revision, sealed as section 3.9.4 defines. The checkpoints are the
history of the draft, as NIP-37 defines.

- `replace` makes the new content the whole of the event.
- `append` reads the current draft, adds the new content at its end, and
  publishes the result. It reads from the store only. `arc sync` brings the
  drafts that the relays hold for an older client (section 7.5).

#### 3.9.2 Content larger than one event

A relay caps the size of one event. Content of at most 32 KiB travels inside
the event itself, so a Nostr client reads it whole. Longer content travels in
parts:

1. The event holds the first 32 KiB, cut at a line end.
2. Core keeps the rest in parts of at most 32 KiB, as events of kind 3275.
   Each part is sealed as section 3.9.4 defines, carries `["-"]`, and names the event's
   coordinate or ID in its `a` or `e` tag.
3. The event gains a tag `parts` that lists the IDs of the parts in order,
   and a tag `lines` that gives the number of line ends in the content and in
   each part. Core keeps the parts first, and the event last.

A body holds at most 256 parts.

#### 3.9.3 Notebook views

A `notebook` action names `kind` and `op`. Operations are `read`, `list`,
`search`, `select`, `next`, `prev`, `index`, and `toc`. The kind must declare
`notebook_index`. `read` supplies `address` and a keyed `d` template. Navigation
supplies `address`; `index` and `toc` supply `notebook`. Optional templates are
`prefix`, `query`, `title`, `page`, `from`, `to`, `order`, `reverse`, `limit`, and
`section`. These are application selections over verified sealed page events.
The command's usual output pipeline renders the resulting records.

A kind with `notebook_entry` keeps each entry of a page. Its operations
also include `history`, `write`, `append`, `fix` and `delete`, with the
templates `text` and `title`.

**The head and the entries.** A page of such a kind has one head and any
number of entries.

- The head is a draft of the notebook kind, with the `d` tag
  `{{address|keyed:<kind>}}`, as before. Its content is the front matter
  and the first text of the page, as a page of an older arc. Its event has
  the tag `["head", "<head id>"]`, 16 random hex digits. `write` makes the
  head once, with its checkpoint. An operation MUST NOT revise a head that
  exists.
- An entry is a draft of the entry kind. Its `d` tag is
  `{{entry|keyed:<entry kind>}}`, where `entry` is `<address>/<id>` and
  `<id>` is 16 random hex digits. A new entry MUST NOT reuse an `<id>`.
- The event inside an entry has the tags `["d", "<address>/<id>"]`,
  `["page", "<address>"]`, `["head", "<head id>"]` and
  `["ms", "<time in milliseconds>"]`. A fix also has `["corrects", "<d>"]`,
  which names the entry that it replaces, or the address of the page.
- An entry has no checkpoint. An operation MUST NOT revise an entry.
- The `ms` tag is the time of the device clock, in milliseconds since 1970.
  It is later than each entry of the page that the device holds, by at
  least one millisecond. The `created_at` is that time in whole seconds.
  A reader that finds no `ms` tag, or one that does not fit `created_at`,
  uses `created_at`.
- The device key signs the draft of an entry, as section 3.9.4 defines.
  The record of an entry names that key as `device`.

**The text of a page.** The text of a page is the content of the head,
then the text of each entry, oldest first: by `ms`, then by `d`.
The newest fix of an entry takes the place of that entry. A fix of the head
replaces the text after its front matter. The `updated_at` field of the
front matter shows the time of the newest entry, or else of the head.
`read`, `select`, `next`, `prev`, `search`, `index` and `toc` use this
text. `read` gives one record, with the field `entries`: the `d`, `at`,
`device` and `corrects` of each entry, oldest first.

An entry belongs to the page whose head has its head id. An entry with no
`head` tag belongs to the head at its address.

The `page` tag is sealed, so the store cannot select the entries of one
page. An operation opens each entry draft of the kind once, and groups the
entries by page. Its cost grows with the number of entries.

**Addresses.** An address of a write operation can end in a word that arc
resolves before it signs:

- `<project>/<notebook>/next` is the page number one higher than the
  highest page number of the notebook, or 1. Only `write` resolves it. A
  page number is at most 999,999,999.
- `<project>/<notebook>/today` is the page with the lowest number whose
  title is today's UTC date, as `YYYY-MM-DD`. `write` and `append` make it
  at `next` when no such page exists. `read` and `history` resolve it, and
  fail when it does not exist.

The words `next`, `today` and `at` MUST NOT name a page.

**Front matter.** `write` fills in each missing field of the front matter:
`notebook` and `page` from the address, and `title` from the `title`
template, else from the first `# ` heading of the text, else from the UTC
date. A field that is given MUST match the address, and a title that is
given MUST match the `title` template.

| Op | Templates | Effect |
| --- | --- | --- |
| `write` | `address`, `text`, `title` | Makes the head of a new page. It fails when the page exists, or when it has no text and no title. A page with entries and no draft of its head exists: its head has not come yet. |
| `append` | `address`, `text` | Adds one entry. It fails when the page does not exist, unless the address is `today`, or when the text is empty. |
| `fix` | `address`, `text` | Adds one entry that corrects the entry `<address>/<id>`, or the head when the address names a page. A fix of a fix corrects the first entry. |
| `history` | `address` | One record for each checkpoint of the head, then one for each entry and fix, oldest first. The record of a fix names the entry that it corrects. |
| `delete` | `address`, `d` | Deletes the head as section 3.10 defines, and each entry of the page and its parts, with one request. It MUST NOT name the head of a lost page at the address: the newest lost page with a head moves up, as below. The delete of a lost page names its head and its entries, and leaves the page at the address. |

`read` and `history` of a page with no head and no entry fail with
`no such page <address>`.

**Lost pages.** Two devices that add entries offline lose no entry. Two
other cases leave heads and entries that the head at their address does
not name:

- Two devices make one new page at one address offline, such as through
  `next`. The store keeps one draft at the address: the newer one, or on a
  tie the one with the lower ID. The checkpoint of the other head stays.
- One device deletes a page while another adds to it. The delete does not
  name the entries that it did not hold.

The head and the entries of each other head id are a lost page. It reads
at `<address>-<head id>` on each device, with no write: its head is the
checkpoint, and a lost page with no head has the entries alone. `ls`
lists it, with page number 0, so the `next` and `prev` operations and a
`next` address pass it. `ls` and `read` say on the standard error why the page is lost.
`append`, `fix`, `history` and `delete` take its address. Its entries
keep the address of the page in their `page` tag, and the id of the lost
head. `fix` also takes an entry by the address of its `page` tag, as
`history` prints it.

When the page at an address is deleted, the newest lost page with a head
moves up: the delete publishes its head as the draft at the address, as it
was written, with its head id, and with no checkpoint. The draft is newer
than the blank draft of the delete. The lost page then reads at the
address, and still at `<address>-<head id>`. A fix of its head that names
`<address>-<head id>` still applies, and the newest fix of the two
addresses wins. Thus a sweep of parts and
checkpoints that no page names, such as the repair of the storage spec,
section 4.7, finds a draft that names the checkpoint, and keeps it.

A lost head with no text after its front matter, and the title of the head
at its address, is not a lost page: its entries join that page. Thus two
devices that start today's page offline keep one page, with each text.

#### 3.9.4 The format of the data roots

A device seals and signs each sealed event with no request to the signer. A
draft, a checkpoint, a part and a deletion have this format:

| Field | Value |
| --- | --- |
| `pubkey` | The device key. A machine that holds the citizen key uses the citizen key. |
| `tags` | The tags of sections 3.9 and 3.10, then `["p", <citizen>]`, `["gen", <n>]` and `["-"]`. A deletion has the `p` tag, and no `gen` tag and no `["-"]`. |
| `content` | The NIP-44 payload of the event inside, with the content key of the capability as the conversation key. A part holds its text. A blank draft and a deletion have no content. |

- The content key is
  `HKDF-SHA256(ikm = data root of generation n, salt = none, info = "arc-data-v1" || 0x00 || capability id)`,
  as section 3.5 of [the devices spec](../devices/SPEC.md) defines. Thus one
  capability cannot open the drafts of another.
- The event inside MUST name the citizen as its author. A checkpoint and a
  part MUST name the draft by the coordinate of the citizen:
  `31234:<citizen>:<d>`.
- A device MUST seal under the newest generation in its root cache, with no
  request to the signer or the relays. `arc sync` and `arc serve` bring the
  root cache up to date.
- The store names such an event by the citizen of its `p` tag. It keeps one
  version of a draft for each citizen and `d` tag, whichever key signed it,
  and applies a deletion of the citizen to each event of the citizen
  ([the delivery spec](../delivery/SPEC.md), section 3.11).
- A reader MUST accept such an event only if the citizen key signed it, or a
  device key on the device list of the citizen. For a revoked device key, a
  reader MUST accept an event only if it came from a storage citizen, its
  `created_at` is earlier than the time of the revocation, and its
  generation is earlier than the generation of the revocation. Section 3.2
  of the devices spec holds the revocations.
- A query of sealed kinds reads the local store only. It asks no relay. A
  page of this format opens with no request to the signer.

A reader still opens a draft of the format before the data roots, which the
citizen key sealed to itself and signed. It opens it through the signer.
Each `arc sync` that reaches the signer seals each such draft again in this
format:

1. The device opens the draft, and each of its parts, through the signer.
2. It seals each part again, and the draft with the new part IDs, under its
   newest generation. The new draft is one second newer than the old one.
   The store therefore keeps only the new draft.
3. The capability of the new draft is the first install that names the kind
   inside as sealed.

A second run finds no draft of the old format, and seals nothing. The old
checkpoints stay, and open through the signer. The keyed root draft,
`d` = `arc-keyed-root`, keeps the old format (section 3.7.1).

#### 3.9.5 The body of a draft

A publish with `body` keeps the raw bytes of a file beside the draft, not
in its content. The content of a draft is a NIP-44 payload, which holds at
most 65,535 bytes, and a part holds text. A file in parts thus costs base64
and one event for each 32 KiB.

```json
"publish": {"kind": "file", "d": "{{file.sha256|keyed:file}}", "revise": "replace",
            "body": "{{file}}", "tags": [["x", "{{file.sha256}}"]]}
```

- `body` MUST name one argument of the type `file`, as `{{name}}`. Only a
  sealed kind has a body. A publish with a body MUST NOT `append`.
- The body key is
  `HKDF-SHA256(ikm = content key of the generation, salt = none, info = "arc-body-v1")`,
  32 bytes. The content key is the key of section 3.9.4.
- The encrypted body is `nonce (24 bytes) || XChaCha20-Poly1305(body key, nonce, raw bytes, aad = none)`.
- The raw bytes MUST be at most 63 MiB (66,060,288 bytes).
- The wrap MUST carry the tag `["body", <SHA-256 of the encrypted body in hex>, <size of the encrypted body>]`.
  The device key signs the wrap, so its signature covers the body.
- The device MUST keep the encrypted body before it keeps the wrap. Thus
  no wrap names a body that the device lacks.
- The checkpoint of the draft has no `body` tag. The history keeps the
  tags of each version, not its bytes.
- A reader MUST check the size and the SHA-256 of the encrypted body
  against the `body` tag before it opens the body with the key of the
  generation of the wrap.
- `save` writes the body of a draft that has one, and ignores its `field`
  and `decode`. A draft with no body saves its field, as before. Thus the
  files app reads a file in parts and a file with a body with one manifest.

The [storage spec](../storage/SPEC.md), sections 3.4 and 3.6, says how a
body travels to the storage citizens and through a directory.

### 3.10 The action delete

`delete` removes events, as NIP-09 defines. It can name only the citizen's
own events of kinds that the manifest names. For a sealed kind, the request
stays in the store, and `arc sync` gives it to the storage citizens. For
another kind, it goes to the relays.

```json
"delete": {"kind": "page", "d": "{{address|keyed:page}}"}
```

For a sealed kind, core publishes the draft with blank content, which NIP-37
reads as deleted. It then publishes a kind 5 request with an `a` tag for the
draft, an `e` tag for the current version of the draft and for each checkpoint
and part, a `k` tag for each kind, and the `p` tag of section 3.9.4. The
device key signs it. The request is one second older than
the blank draft, so it removes every older version, and the blank draft stays.
The store and each other device therefore drop every version, as NIP-09
defines. The store also refuses each of these events after the request, so a
stick or a storage citizen does not bring the page back.

A new version of a draft is always newer than the version before it, by at
least one second, so it replaces that version.

### 3.11 The action query

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

A query of a sealed kind reads the drafts in the store, and filters them by
their `k` tag. It also asks the relays, which hold only the drafts of an older
client (section 7.5).
A query of a private kind first syncs the mail with each relay, then reads the
messages that this citizen received and sent, from their seals. A query of a
group kind reads the group's relay, and not the store: a post that an admin
removed does not show, and a query fails when the group's relay does not
answer.

### 3.12 The action watch

`watch` is a query that stays open. It runs for sealed, public and group
kinds. It does not run for private kinds yet. It runs the pipeline on each event that
matches, first on the stored events, then on each new one as it arrives. It
ends when the citizen stops it, or when every relay ends the subscription.
A watch of a sealed kind reads the store each 250 ms, and no relay: it shows
each change of a command on this machine, and each change that a sync
brings.

### 3.13 Records

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

### 3.14 The output pipeline

A command lists its output primitives. Core runs them in this order, whatever
the order in the manifest:

| Order | Primitive | What it does |
| --- | --- | --- |
| 1 | `open` | Opens drafts and private events, and parses content as `text`, `json`, or `frontmatter` into fields. |
| 2 | `join` | Puts the whole content of each record in `text`, parts included. See section 3.14.1. |
| 3 | `where` | Keeps records whose fields match. |
| 4 | `latest` | Keeps the newest record for each value of a field. |
| 5 | `rank` | Orders records by BM25 against a query, over named fields. |
| 6 | `thread` | Orders records as replies, by a field that names the parent. |
| 7 | `sort`, `limit` | Orders by a field, and cuts. |
| 8 | `tail` | In a watch: shows only what each new version adds. See section 3.14.3. |
| 9 | `save` | Writes a field to a `path` argument, instead of showing it. A draft with a body writes its body (section 3.9.5). |
| 10 | `format` | Shows each record with a named format, see section 3.15. |
| 11 | `exit` | Sets the exit status of the command from its first record. See section 3.14.4. |

```json
"output": {
  "open": {"parse": "text"},
  "join": {"lines": "{{lines}}"},
  "format": "page"
}
```

#### 3.14.1 join

For each record, `join` writes the content inside the event, then the content
of each part that its `parts` tag lists, in order. It fetches the parts from
the store, then from the transports.

If a listed part is missing, `join` stops after the text before it, and
reports the missing part. With `lines`, `join` reads the `lines` tag, and
fetches only the parts that hold those lines. When `join` is the last primitive before `format`, it writes each
part as it arrives.

#### 3.14.2 where, latest, rank, thread, sort

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

#### 3.14.3 tail

In a watch, `tail` compares each new version of a record with the version
that it showed before. When the new text starts with the old text, `tail`
shows only what follows it. Otherwise it shows a line that says the record was
rewritten, and then the whole new text.

#### 3.14.4 exit

`exit` sets the exit status of the command, after the output is shown. It is
a list of rules. Core reads the first record that the pipeline keeps, and
takes the first rule whose `where` conditions that record meets. A rule with
no `where` meets every record.

```json
"exit": [{"where": [{"field": "state", "is": "running"}], "code": "75"},
         {"code": "{{exit}}"}]
```

- `code` is a template over the record. A whole number from 0 to 255 is the
  exit status. Any other value, and a missing field, gives the status 1.
- With no record, or with no rule that fits, the status is 0.
- A call that waits in the outbox has no reply, so it has no exit status.
- `--json` writes the record, and the command still exits with the status.
- A command that names a list runs once for each member. A member whose
  status is not 0 counts as failed, and the command then exits 1.
- A watch cannot have `exit`.
- An error of `arc`, for example no relay, exits 1 and writes a message.

A manifest with `exit` needs a caller of v0.12.0 or later. An older caller
refuses the manifest, because it does not know the field.

### 3.15 Formats

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

arc removes from each value, before it shows it, each character that
[the clients spec](../clients/SPEC.md), section 3.9, escapes, except a
backslash: a control character other than newline and tab, an embedding,
override or isolate, a line or paragraph separator, an invisible character,
and a tag character outside the three subdivision flags. A value from an
event cannot move the cursor, change the terminal, reorder a line, or hide
text. A program that reads the output through a pipe gets the rest of the
value as it was, with its backslashes. arc removes the same characters from
the text of a format template, which the author of the app wrote, and from
a reply that does not fit the output of its app. An older arc shows the
text of a template as the manifest holds it. The first time that it
removes a character in a command, it says so on standard error. The error
of each member of a list goes to standard error on one line, escaped.

### 3.16 Flags of every command

| Flag | Meaning |
| --- | --- |
| `--json` | Writes each record as one JSON object on one line, after the pipeline and before `format`. Agents read this. |
| `--dry-run` | For `publish` and `delete`: writes each event that core would sign, and signs nothing. |
| `--later` | For a live `call`: waits in the outbox instead. |

### 3.17 Keys

A citizen's key comes from one of these, as NIP-19, NIP-49 and NIP-46 define:

| Source | Meaning |
| --- | --- |
| `nsec` | The secret key, in a file that only its owner can read. |
| `ncryptsec` | The secret key, encrypted with a passphrase. Core asks for the passphrase. |
| `bunker://` or a NIP-05 name | A NIP-46 remote signer. Core never holds the secret key. An agent signs through a signer that its owner controls. |

Each identity of a machine has its own directory, `<home>/citizens/<name>`,
with its key, store, relays and installs.

arc picks its home in this order:

1. `--home <dir>`.
2. `ARC_HOME`.
3. `~/.arc`, if it holds an entry of arc.
4. `~/.config/arc`, if it holds an entry of arc. An older arc made its home
   there, and it stays in use.
5. `~/.arc`, for a new home.

The entries of arc are `citizens`, `default`, `device`, `relay.db`, `wake`
and `wake.toml`. An app that keeps its data in `~/.arc/<app>` MUST NOT use
one of these names, and `arc install` refuses them as the name of an app.
arc MUST NOT move a file between the two homes. arc MUST NOT look for a
home in the working directory or a parent of it: a cloned repository can
hold a `.arc` with the key of someone else. If arc cannot read a home of
steps 3 and 4, for example a folder of mode 000 or a folder that root owns
after `sudo arc`, arc MUST fail with an error that names the folder. It
MUST NOT go to the next step: that would use, or make, another identity. If
`~/.arc` and `~/.config/arc` both hold an identity, and arc uses `~/.arc`
by step 3, each command MUST warn once on the standard error, and name both
folders. An older arc, or an older arc-dm, uses the identity of
`~/.config/arc` on the same machine. If arc cannot read the `default` file
of its home, `arc keys gen`, `arc keys recover` and `arc keys add` MUST fail
before they write a key: else arc keeps a key whose seed nobody saw. `arc keys gen` makes an identity
from a new 24-word seed, as docs/delivery/SPEC.md section 3.2 defines.
`arc keys recover` makes it again from the words, and `arc keys add` adds a
key that exists already. The first identity is the
default. `arc keys use <name>` changes the default, and `--key <name>` or
`ARC_KEY` picks another identity for one command.

`arc keys gen --encrypt` and `arc keys encrypt` seal a key with a passphrase.
Core reads the passphrase from `ARC_PASSPHRASE`, or asks on the terminal.

Remote signer setup takes at most 15 seconds. Each operation, including
NIP-04 encryption and decryption, takes at most 20 seconds. An earlier caller
deadline applies. Cancellation stops setup and remains identifiable in returned
Go errors, as it does for a local signer.

`arc keys bunker --relay <url>` serves this citizen's key as a NIP-46 signer,
and prints its `bunker://` URI. With `--allow-kind`, it signs only those kinds,
and NIP-42 authentication for relays. `--decrypt` says what it opens: `none`;
`self`, the default, which opens only what the owner sealed to their own key,
such as drafts and the keyed root; or `all`, which mail needs, because a gift
wrap comes from a one-time key that no list can name. The bunker keeps its URI
when it restarts. When the citizen has a device list, the bunker admits only
the device keys on it, and its URI holds no secret. `arc keys device`,
`arc keys devices`, `add` and `revoke` manage the list, as
[the devices spec](../devices/SPEC.md) defines. `arc keys add <uri>` makes an identity that
signs through it. That identity seals and opens mail, makes calls, and reads the
keyed root, all through the signer. The bunker publishes the keyed root to its
relay when it starts. If it refuses to open locally recorded outgoing mail,
an inbox or outbox command reports the error instead of treating those
records as absent.

### 3.18 Addresses

An address names a capability, its provider, and one resource of it:

```text
<scheme>+arc://<provider>/<path>
exec+arc://npub1.../
sqlite+arc://<64-hex-key>/main
```

- The scheme names the capability: the capability whose id, the `d` tag of
  its announcement, is the scheme. Else, the only capability whose manifest
  has that scheme. The scheme holds lower-case letters, digits and hyphens.
- The provider is a public key: 64 hex characters, an `npub`, the petname
  of an installed provider, or an installed name. A domain stands for the
  NIP-05 name `_@<domain>`. A NIP-05 name with a local part cannot stand in
  an address, because its `@` is user information.
- The path is the path of the request. It replaces the path of the manifest.
  Without a path, the path is `/`. It holds ASCII letters, digits, and
  `/ . _ ~ -`.
- An address has no user information, port, query, fragment, percent
  escape, or dot segment. Core refuses such an address before any call.

`arc call <address> [body]` sends the body to the capability that the
address names, and shows the reply with the `output` of the service, see section 3.4.
`--raw` writes the reply as it came. The capability must be installed, as for every call. The
address carries no trust: the key of the provider and the install do.
`--capability <id>` names the capability when two have one scheme, and its
scheme must then match the address.

### 3.19 Provider bundles

New bundles contain one authored interface manifest in `manifest.json`.
`arc serve` also accepts a modern manifest at another explicit path. Historical
bundles can still use a legacy manifest with an adjacent `interface.json`;
the modern document takes precedence even if the legacy file is missing or
invalid. Loading produces one validated definition for the announcement,
capability ID and request limit. An omitted or zero `service.max_bytes`
normalizes to 1 MiB in both the announcement and the host.

## 4. Behavior

### 4.1 Install and dispatch

`arc install <author> [capability]` reads the announcement, verifies it, shows
what section 6 requires, and asks the citizen once. The capability's name is
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

### 4.2 Calls of a provider

A provider program can call a capability, as the citizen that serves it.
`arc serve` makes the call. The provider program never holds the key of the
citizen. A web service keeps its data in `sqlite` this way, for example.

The rules are the rules of `arc call`:

- The citizen must have installed the capability. If not, the call fails
  with `not_installed`. An install is the consent to call.
- The call names the capability by an address, see section 3.18. The method is the
  method of the manifest.
- The call is live. It needs a relay and has at most 115 seconds, shortened
  by the remaining deadline of its parent request.

`arc serve` and the provider program speak newline delimited JSON on the
standard input and the standard output of the program. The program writes
one line for each call:

```json
{"op": "call", "call_id": "7", "address": "sqlite+arc://<key>/main", "body": "{\"sql\": \"select 1\"}"}
```

`arc serve` writes one result with the same `call_id`:

| Result | Meaning |
| --- | --- |
| `{"op": "result", "call_id": "7", "reply": "..."}` | The reply of the provider that got the call. |
| `{"op": "result", "call_id": "7", "refused": "unauthorized"}` | That provider answered with an error. |
| `{"op": "result", "call_id": "7", "error": "not_installed: ..."}` | `arc serve` could not make the call. |

In Go, a handler that has the method `SetCaller(provider.Caller)` gets a
caller before its first request. `Caller.Call` returns the reply, or a
`*provider.CallError`. Its field `Refused` separates a refusal from a
failure.

### 4.3 Deadlines, cancellation and admission

Live calls have one budget of at most 120 seconds, including wake, relay
discovery, relay selection and execution. `arc call --timeout` can shorten it. The signed
request rumor carries the absolute Unix millisecond deadline in a `deadline`
tag. A queued request omits it; its execution budget begins when it is served.

The host passes the deadline to the provider as `deadline_ms` on the request
line. An outbound `call` line carries the remaining deadline in the same field.
A missing deadline uses the host's 120-second cap. Go handlers receive a
context with the earlier of their parent and supplied deadlines. The runtime
rejects expired requests before dispatch. A canceled outbound call does not
write a new call to the host. Context failures return `provider_timeout` or
`provider_canceled`, rather than `internal_error`.

The host cancels a request with
`{"op":"cancel","request_id":"..."}`. A provider cancels an outbound call with
`{"op":"cancel","call_id":"..."}`. Go handlers must stop work when their
context ends. EOF cancels active handlers before the runtime joins them and
allows up to one second to drain their final replies. An unread output stream
cannot keep shutdown waiting. When an outbound call is canceled, an active
write has up to one second, within its existing deadline, to finish. A complete
line can reach the host before its write reports success; that completion
must preserve the stream and allow the cancellation notice to follow. A write
that remains blocked is interrupted. A cancellation notice has at most one
second to write.
Waiting for another writer respects the caller's context without interrupting
that writer. An interrupted or failed write stops the output stream and the
runtime, because another JSON line cannot safely follow a partial line.
Blocking custom Go output writers must implement `io.Closer` so `Close` can
interrupt `Write`. Output is serialized with at most one active write.
Intentionally detached jobs retain their explicit job timeout.

The Go runtime admits at most 64 concurrent handlers by default, configurable
with `provider.Options.MaxConcurrent`. Excess requests receive `provider_busy`.
Cancellation and result messages bypass handler admission.
The Go runtime writes rejection replies through one worker with a queue of at
most 64 pending replies. Blocked rejection output cannot hold up cancellation,
results, or EOF. A full rejection queue stops the stream with `provider_busy`.
Earlier rejections finish before a later admitted request starts its handler.
The host separately bounds incoming live work and outbound calls. Exec drains
output while keeping only bounded prefixes; output volume cannot grow its
in-memory buffers without limit.

### 4.4 Core sessions

A service can declare `service.interactions` with `request_reply`,
`server_stream` and/or `duplex`. Omission or an empty list keeps request/reply.
Unknown and duplicate modes are errors. Ordinary call commands require
`request_reply`. A service declaring a streaming mode may omit command entries;
`arc session <address>` provides its interactive entry point.

`arc session <address> [initial request] --mode duplex` streams stdin and stdout
through the same core used by provider handlers. Input EOF is a half-close, and
the command waits for the provider's final status. `--mode server_stream` sends
only the initial request and reads incremental output. Installation consent and
provider access rules still apply. A provider can consume another session using
`provider.SessionCaller`, supplied through `SetSessionCaller`.

See [the session protocol](../sessions/SPEC.md) for frame limits, admission,
cancellation, deadlines and explicit disconnect behavior. Existing `arc call`
and queued request/reply operations retain their protocol.

## 5. Failures

Each rule of section 3 names its own refusal. These are the limits of this
design that a user meets as a failure or a delay:

- **Rollback on a fresh machine.** A query asks every relay of the citizen's
  NIP-37 list, and keeps the newest version. If every relay serves an old
  version, a machine that never saw the newer one cannot tell.
- **One process per home.** The store is one file that one process opens at
  a time. A `tail` holds it, so a second command on the same home waits.
- **Each read of public data asks the relays.** A query fetches from every
  relay before it reads the store, so a slow relay makes every read slow. A
  read of sealed data reads only the store.
- **Sealed data syncs by a full fetch.** A Negentropy session opens its own
  connection, which cannot answer the relay's challenge, so `arc sync` fetches
  sealed data whole instead of comparing sets.
- **A remote signer needs the keyed root first.** A machine with the key must
  run once, and reach a relay or a stick that the remote machine reads, before
  the remote machine can use a capability that uses `keyed`.
- **Each seal and each opened message is one request to the signer.** Mail
  over a remote signer is as slow as the round trips to it. A journal page
  needs no request (section 3.9.4).

## 6. Security

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
| 3276 | Core live-session interaction frames. |
| 1234, 31234, 3275 | Checkpoints, drafts, and parts. Core makes them for sealed kinds. |
| 3272, 3273, 3274 | Calls and acknowledgements. Core makes them. |
| 9734, 9735 | Zaps. |
| 10002, 10013, 10050 | Relay lists. |
| 10272, 30272 | A kind that ARC keeps unused, and announcements. |
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

**Sealed data is served only to its author.** A relay that `arc` runs, and
`adapters/relay/sealed` protects, answers a query that names a draft, a checkpoint,
a part or a private relay list only after NIP-42 authentication, and only
when the query names the authenticated citizen as its only author. Every
other query leaves out the sealed events of other citizens, so nobody else
learns how many drafts a citizen has, or when they wrote them.

**Keyed values stay inside a capability.** See section 3.7.1.

**A reply is data.** Core never runs, opens, or follows what a reply or an
event holds.

The limits of this design that bear on security:

- **The `k` tag of a draft is public.** A relay learns that a draft holds an
  article, a KPI series, or a file, but not its content or its address.
- **A revoked device can backdate a page.** It keeps its device key and the
  data roots that it held, so it can sign a page with a `created_at` before
  its revocation. Another device takes such a page only from a storage
  citizen, which refuses the device key after the revocation. A hostile
  storage citizen that works with the thief can still give it. Two storage
  citizens show the difference.
- **Mail over a bunker opens everything.** `--decrypt all` lets the machine
  that uses the bunker open any ciphertext sealed to the owner, not only mail.
- **Encryption is not limited.** A bunker encrypts for its client without a
  rule, because a sealed message also needs a signature, which `--allow-kind`
  limits.
- **Another relay may serve drafts to anyone.** Only a relay that enforces
  NIP-42 reads, as `arc relay serve` does, keeps the events themselves from
  others. The content stays encrypted on every relay.

## 7. Compatibility

### 7.1 Versions

This interface is version 1. Core refuses a manifest of a later version, and
says which version it would need. A new primitive, or a new field with a new
meaning, comes in a new version. The older stack's interfaces, versions 1 to
4, do not run on the delivery layer.

### 7.2 What leaves core

| Today | In version 1 |
| --- | --- |
| The `journal` package | The journal manifest: NIP-37 drafts of NIP-23 articles. |
| The direct-message renderers and cache in `toolbox` | The program `arc-dm` of the dm app, over `arc message`: NIP-17. |
| The `agora` input source | No manifest. The Agora app was removed, because its relay kept its data. |
| The `private_file` and `sealed_file` inputs | The files manifest: NIP-37 drafts of NIP-94 file metadata. |
| The `seal`, `pubkey` and `shell` filters | `key` arguments, private kinds, and `json`. |

### 7.3 The apps in version 1

Each app of version 1 documents itself, and its `manifest.json` is its
interface. See [the apps](../../apps/README.md): [exec](../../apps/exec/README.md),
[sqlite](../../apps/sqlite/README.md), [releases](../../apps/releases/README.md),
[journal](../../apps/journal/README.md) and [files](../../apps/files/README.md).
The [dm app](../../apps/dm/README.md) has no manifest: its program `arc-dm`
drives `arc message`.

### 7.4 Sealed data moved off the relays

Before this change, a sealed event went to the relays of the citizen's NIP-37
list, kind 10013, and `arc sync` reconciled it with each relay. ARC no longer
makes that list. Now it stays
in the local store, and syncs with the storage citizens of the citizen. The
[storage spec](../storage/SPEC.md) defines the sync. Section 7.5 says how an
older client behaves.

### 7.5 An older client

- An older arc uses `~/.config/arc` and never `~/.arc`. A machine that an
  older arc set up keeps its home in `~/.config/arc`, and a newer arc uses
  it (section 3.17). A home that a newer arc made in `~/.arc` is not found
  by an older arc: give it `--home ~/.arc` or `ARC_HOME`.
- An older client keeps writing sealed events to its relays. `arc sync` of
  a newer client imports them, and seals each one again in the format of
  section 3.9.4 when it reaches the signer. Thus the newer client sees the
  pages of the older client.
- An older client cannot open a page of the format of section 3.9.4. Its
  store keeps the event as data of the device key, and its query of the
  citizen does not find it. An older client that runs `arc sync` refuses
  such a blob. After its update, it fetches each blob once more
  ([the storage spec](../storage/SPEC.md), section 7), and reads the page.
- A newer client opens a page of the older format through the signer. With
  the signer stopped, such a page does not open until an `arc sync` with
  the signer seals it again.
- An older client does not see the pages of a newer client, because they do
  not go to a relay. The user updates each device.
- A newer client deletes the drafts on its ARC relays after a storage
  citizen holds them (storage spec, section 4.2.1).
- An older client cannot parse a manifest with `body` (section 3.9.5). Its
  catalog skips the announcement, so `arc install` of the files app says
  `no announcement from that provider`. An older client refuses the blob
  of a file with a body, and gets it after its update
  ([the storage spec](../storage/SPEC.md), section 7). A newer client
  reads a file that an older client put in parts.
- An older client cannot parse a manifest with `notebook_entry`
  (section 3.2). Its catalog skips the announcement, so `arc journal` stops
  on that home until it updates. Announce such a manifest only after each
  home updates. The kind of the entries is new, so after the announcement
  each home agrees to it again with `arc install` (section 4.1).
- An older client cannot read an entry. Its store keeps the draft, and its
  queries do not find it, so it shows a page without its entries. A newer
  client reads each page of an older client: its text is the head.
- An ARC relay refuses new sealed events, and still serves the old ones to
  their author until 2027-01-10. An older client then gets
  `blocked: this relay keeps no drafts` for each new page. The storage spec,
  section 7, says how older clients and older relays behave.

### 7.6 Limits of the Nostr standards

- **Parts are ARC's own.** No NIP carries private content larger than one
  event. A Nostr client reads the first 32 KiB of a longer page, and no more.
- **NIP-37 is a draft.** Like NIP-17, it can still change. ARC follows its
  text as of this document.
- **No Nostr client opens a page.** The content key comes from the data
  roots, which only the devices of the citizen hold. ARC keeps the shape of
  NIP-37, so the kinds, the `d` tag and the `k` tag still follow it.
- **A private event goes to at most 9 recipients.** A private publish can
  name several recipients. The mail layer seals one rumor to each, as
  [the group messages spec](../groups/SPEC.md) defines. The manifest gives
  the tags: a manifest that publishes kind 14 to several recipients MUST
  give a `p` tag for each.
  NIP-17 advises another scheme past 10 participants, with the author. An
  arc older than group messages refuses a publish with more than one
  recipient.

## 8. Proof

`mise run interface` proves phases A to D. `TestDevicePages` proves phase E, `TestStorageLargeFile` proves phase F, and the entry tests of `runtime/iface/entries_test.go` prove phase G.

| Phase | Scope | Proof |
| --- | --- | --- |
| A (built) | The manifest reader, arguments, templates, `call`, and `format`. NIP-19 keys and events. exec, sqlite and releases in version 1. | `arc exec run echo hello` answers through the installed manifest. |
| B (built) | Sealed kinds: NIP-37 drafts, checkpoints, NIP-70, parts, `delete`, kept in the local store. `open`, `join`, `where`, `sort`, `tail`, `save`. The journal and files in version 1. | A journal page and a file cross a storage citizen and a USB stick. The `journal` package is gone. |
| F (built) | The body of a draft (section 3.9.5): a file of up to 63 MiB as one body of raw bytes. | `TestStorageLargeFile`: a file of 60 MiB crosses three storage citizens and a USB stick with the same SHA-256; a file in parts of v0.30.0 still reads. |
| E (built) | Pages of the data roots: the device key signs, the content key seals, the store names a page by the citizen, the import of old pages. | `TestDevicePages`: with the signer stopped, a device writes and reads its journal; a second device reads the page after a sync; a page of a revoked device after its revocation is refused; old pages are sealed again once. |
| C (built) | Private kinds through the mail layer, `watch`, `rank`, `latest`, `thread`. Group kinds. The test manifest `runtime/iface/testdata/dm.manifest.json`. | A direct message opens in a NIP-17 client. A list of two citizens gets one message each. `go test ./runtime/iface` posts group kinds with the `h` and NIP-70 tags. |
| D (built) | Install consent, reserved kinds, `--dry-run`, `--json`, and keys from `ncryptsec` and NIP-46 signers. | A manifest that names a reserved kind does not install. A new kind asks the citizen again. An agent signs through a remote signer. |
| G (built) | Notebook entries (section 3.9.3): a head written once, entries of kind 4272 that are never revised, fixes, `next` and `today`, front matter that arc fills in, and no wait for a terminal. | `TestTwoDevicesThatAppendOfflineKeepBothEntries`: two devices append to one page offline, and after a sync each reads both entries in time order. The same test on the code before entries loses the entry of the first device. |
