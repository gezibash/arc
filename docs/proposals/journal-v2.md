Status: proposed

# Journal v2: entries, links, sharing and redact

No code implements this proposal. Each claim about ARC names a file or a
spec section at commit `16c08bb`. Each number has a tag:

- *measured*: a run on a real machine. The source is given.
- *arithmetic*: a calculation from other numbers. The inputs are given.
- *estimated*: a judgement. No run supports it.

Building waits until Zim and Otto have read this document.

## 0. Bad news first

- **A new journal announcement stops `arc journal` on each home that runs an
  older arc, at its next sync.** Phase A adds fields to the journal
  manifest. An older arc refuses a manifest with an unknown field
  (`runtime/iface/manifest.go:161`). Each `arc sync` pulls every
  capability announcement (`cmd/arc/capability.go:39`, `cmd/arc/main.go:998`).
  The store keeps one version of an addressable event, such as an
  announcement of kind 30272 (`core/store/store.go:230`). `catalog.Find`
  skips an announcement that it cannot read, and then says
  `catalog: no announcement from that provider` (`runtime/catalog/catalog.go:244-251`).
  So every home updates before the journal is announced again (section 8).
- **The link form that was asked for is not an ARC address.**
  `arc://journal/<owner>/<project>/<notebook>/<page>#<entry>@<time>` breaks
  three rules of [the interface spec](../interface/SPEC.md), section 3.18:
  the scheme goes before `+arc://`, an address has no fragment, and a path
  holds no `@`. Section 4.5 keeps the address form of ARC instead:
  `journal+arc://<owner>/<project>/<notebook>/<page>`, with the entry and
  the time as path segments.
- **A revoke takes nothing back.** A reader keeps each entry that reached
  them. They can copy it, and a clone (section 5.2) only makes that copy
  official. A revoke stops new entries only.
- **Redact reaches only the stores that obey it.** It removes the text from
  this device, the storage citizens and the stores of readers whose arc is
  new enough. It cannot remove a copy that a reader made outside ARC, a
  clone, or the store of a device that is offline or runs an older arc. A
  storage citizen deletes on request, and nobody can prove that it did.
- **Redact needs a working delete path first, and today there is none.**
  Cato's test of v0.36.0 (2026-10-19, *measured*): he wrote a page, deleted
  it before any sync, and then ran `arc sync`. The sync sent 132 records,
  and the use on each storage citizen went from 1.4 MiB to 5.3 MiB.
  `arc sync --repair` removed 0 blobs. Section 6 traces the cause. About
  4 MiB of his test data stays on the shared storage citizens until it is
  fixed.
- **The largest page is about 3 MB, not the 256 parts of section 3.9.2.**
  A page of 3.0 MB works, and a page of 4.04 MB fails with
  `store: content is too large: 65628, max is 65535` (Cato, 2026-10-19,
  *measured*). The Bolt store codec caps the content of one event at
  65,535 bytes (`eventstore/codec/betterbinary/codec.go:93` of the
  nostrlib fork). The draft of a page holds its first 32 KiB and the list of
  its parts, sealed. Past about 100 parts, the draft is larger than the cap
  (*estimated*). Entries (section 4) are small, so phase A removes the
  problem for new writing. An old page that is too large stays too large.
- **The time of receipt on a storage citizen is a second clock, not a
  proof.** A storage citizen can lie about it. It changes the storage
  protocol (section 5.4).
- **Phase B, redact, attachments and device signing touch keys.** Zim
  decides them directly (section 9).

## 1. Purpose

Zim and Otto set the scope on 2026-10-19 (11:02 and 11:26 Europe/Tirane).
The journal becomes a log of entries that nobody can change, which a
person or an agent writes and links, and which an owner can share:

1. Append-only entries. A fix is a new entry that names the entry it
   corrects. A reader sees the latest state, with the history one command
   away.
2. A write time on each entry, signed by the device of the writer, and a
   time of receipt from the storage citizen. Lists go by time. Page numbers
   stay as labels. A clock that is far wrong shows.
3. Sharing of a notebook or a page, read only and live, to a named person
   or by a link with its own key. A revoke stops new entries.
4. Cloning of what was shared: an own copy, no longer linked, that records
   its origin.
5. Page links with one canonical address, `[[...]]` and Markdown links,
   `journal open` and backlinks with `journal links`.
6. No command needs a TTY, so agents can run each one.
7. Redact of an entry by its owner, everywhere, with a marker that stays.
8. Full-text search over own and shared notebooks, with the index on the
   device only.
9. Open items: `- [ ]` and `- [x]`, and `journal todo`.
10. One timeline: `journal log --since 2d`, with filters.
11. `--json` on each command, and `@<name>` mentions that notify over ARC.
12. Several devices of one owner write to one notebook. Each entry names its
    device.
13. Attachments, encrypted on the storage citizens, in the owner's quota.
14. Templates, such as `journal add --template decision`.
15. Export to Markdown or HTML, with links that work offline.

Zim also folded the "easy journal" change into phase A (11:26), so the
interface spec gets one breaking change and one release, not two:

- the addresses `<project>/<notebook>/next` and `<project>/<notebook>/today`,
- front matter that arc fills in,
- `journal new` and `journal add`,
- no TTY.

Cato's findings 2 to 4 of 2026-10-19 also go in phase A: `--title` does not
work, the size limit has no clear message, and `journal read` of a missing
page prints nothing and exits 0.

## 2. Terms

- **Owner**: the citizen whose journal it is. Only the owner writes.
- **Notebook**: `<project>/<notebook>`, as today.
- **Page**: `<project>/<notebook>/<n>`, a label that groups entries.
- **Entry**: one piece of text that a device wrote to a page at one time.
  It never changes. Section 4 defines it.
- **Reader**: a citizen or a link that a notebook or page is shared with.
- **Share key**: a key that seals the entries of one share.

## 3. What the current design cannot do

| Item | How it works at `16c08bb` | What is missing |
| --- | --- | --- |
| 1 Append-only | A page is one event of kind 30023 inside a NIP-37 draft of kind 31234 ([interface](../interface/SPEC.md), section 3.2). `append` reads the draft, adds the text and publishes the whole draft again (section 3.9.1). Checkpoints of kind 1234 keep each revision. | An entry that nobody can change, and a link from a fix to the entry it corrects. |
| 2 Times | `created_at` and `updated_at` are managed by arc (`apps/journal/README.md`). The device key signs each draft (section 3.9.4). | The storage citizens stamp no time of receipt ([storage](../storage/SPEC.md)). Lists go by page or by `created_at`. |
| 3, 4 Sharing, cloning | The content key comes from the data root of the citizen and the capability id (section 3.9.4). Only devices of the owner hold it. | A key per share, sealing to a reader, revocation, a clone with its origin. |
| 5 Links | `index` and `toc` name headings of pages (section 3.9.3). | Parsing of `[[...]]` and Markdown links, `open`, backlinks. |
| 6 No TTY | `write` takes standard input. Its front matter must name `title`, `notebook` and `page` (`apps/journal/manifest.json:8`, interface section 3.2). | A write with no front matter. `--title` only sets a tag (`manifest.json:41`), so a write without a header always fails with `frontmatter: requires nonempty "title"`. |
| 7 Redact | `delete` publishes a blank draft and a kind 5 request (section 3.10). | A marker that stays, the reach into shares, and a delete that frees the storage citizens (section 6). |
| 8 Search | `search` runs over the owner's own pages (section 3.9.3). | Shared pages in the index. |
| 9, 10 Open items, timeline | No notebook op reads items or merges notebooks. | `todo` and `log`. |
| 11 `--json`, mentions | `--json` exists on each command (section 3.16). A mention is plain text. | A notification over ARC. |
| 12 Devices | The device key signs each draft (section 3.9.4). Two devices that append to one page at once each publish a whole draft. The newer one replaces the other, so one append is lost (*estimated*, from section 3.9.1). | Entries, which do not conflict. |
| 13 Attachments | The files app keeps sealed files ([files](../../apps/files/README.md)). | A link from an entry, and a key for a reader. |
| 14, 15 Templates, export | None. | All of it. |
| — Missing page | `journal read` of a page that does not exist prints nothing, and exits 0: the format `page` has no `empty` text (`manifest.json:13`). | `no such page`, exit 1. |
| — Size | The draft of a page cannot pass 65,535 bytes of content (section 0). | A clear message with the size and the limit. |

## 4. Phase A: entries, time order, links, redact and the easy journal

### 4.1 An entry

An entry is a sealed event of its own. It uses the draft format of
[the interface spec](../interface/SPEC.md), section 3.9.4, unchanged, so the
storage sync and the data roots need no change. An entry is never revised:

- Its `d` tag is `<project>/<notebook>/<page>/<id>`, where `<id>` is 16
  random hex digits. A new entry never reuses a `d`, so the store keeps
  each entry, and no entry replaces another.
- It has no checkpoint, because it has one version.
- The inner event names the page in a tag `["page", "<project>/<notebook>/<page>"]`.
- A fix has a tag `["corrects", "<d of the entry that it fixes>"]`. A reader
  shows the newest fix in place of the entry, and `journal history` shows
  every version.
- Its `created_at` is the write time, by the clock of the device. The
  device key signs the draft, as today, so each entry names its device.

The inner kind is a decision for review: a new regular ARC kind, or kind 1
of NIP-01 inside the draft. The row in [the kind registry](../KINDS.md) goes
in the build PR, because the registry test needs the kind in code.

A page is the list of the entries that name it, in time order. A page of
v1 stays readable. Its whole text shows as one old entry.

### 4.2 Time order

`journal ls` and `journal read` list entries by `created_at`. Page numbers
are labels only. Section 5.4 adds the time of receipt.

### 4.3 The easy journal

- `journal new <project>/<notebook>` writes a new page at
  `<project>/<notebook>/next`: the highest page number plus one. It prints
  the address.
- `journal add <project>/<notebook> <text>` adds an entry to
  `<project>/<notebook>/today`: the page whose title is today's UTC date,
  else a new page with that title.
- arc fills in missing front matter: `notebook` and `page` from the
  address, and `title` from the first `# ` heading, else the date. A field
  that is given must still match, as today. `--title` fills the header, so
  Cato's finding 2 is fixed.
- No command waits for a TTY. A command with no text and no standard input
  says so and exits 1.

These need a runtime addition in `runtime/iface` only:

- an address form for a publish, `next` and `today`, resolved from the
  citizen's own pages before signing,
- a kind option that fills in front matter.

### 4.4 Redact

The owner, or a device of the owner, signs a redact of one entry. It is the
only exception to append-only, and it is for leaked secrets.

- The store removes the text of the entry, and keeps a marker:
  `redacted by <owner> at <time>`.
- `arc sync` sends the redact to each storage citizen, which deletes the
  blob. This depends on section 6.
- Phase B sends the redact to each reader of the entry.

### 4.5 Links

The canonical address of a page is
`journal+arc://<owner>/<project>/<notebook>/<page>`. It follows
[the interface spec](../interface/SPEC.md), section 3.18. An entry adds the
segment `<id>`, and a time adds `at/<unix seconds>`:

```text
journal+arc://<owner>/arc/otto/3
journal+arc://<owner>/arc/otto/3/9f2c4e1a0b7d3c55
journal+arc://<owner>/arc/otto/3/at/1792411200
```

Inside an entry, arc accepts `[[arc/otto/3]]` for a page of the owner,
`[[<owner>:arc/cato/1]]` for a page of another citizen, and
`[text](journal+arc://...)`. It saves each as the canonical address.

- `journal open <link>` shows the page.
- `journal links <page>` lists the pages that link to it, from the local
  index.
- A link to a page that the reader cannot read says
  `you cannot read this page`, exits 0, and shows nothing of it.

### 4.6 Interface spec changes

All of them change normative sections of a `built` spec, so the build is one
breaking commit:

- Section 3.2: a kind option that fills in front matter.
- Section 3.9: the address forms `next` and `today` for a publish, and a
  `revise` value `entry` (a new `d` each time, no checkpoint).
- Section 3.9.3: the notebook ops `entries`, `links` and `todo`.
- Section 3.10: the action `redact`.
- Section 3.18: the segments of an entry and a time in a journal address.

An older arc refuses the new manifest, as section 0 says. It also cannot
open an entry. Its store keeps the event, and its queries do not find it. A
newer arc reads every page of v1.

### 4.7 Phase A PRs

- **A0**: the delete fix of section 6. It is a prerequisite.
- **A1**: entries, the easy journal, time order, `--json`, and Cato's
  findings 2 to 4. This is the breaking change, then the release, then the
  new announcement.
- **A2**: links and redact.

Otto and Cato test each PR as users.

## 5. Phases B to D

These sections give the decisions, not the design. Each phase gets its own
proposal section before its build.

### 5.1 Phase B: sharing

- A share has its own share key. The owner seals each new entry of the
  shared notebook or page to that key, as well as to its own content key.
- **To a person**: the share key goes to the reader sealed to their key, in
  mail.
- **By a link**: the link holds the share key. Each link has its own key,
  so a revoke of one link leaves the others.
- **Revoke**: the owner makes a new share key for the readers who remain,
  and stops sealing to the old one. A reader keeps what they got. The docs
  say so plainly.
- The entries of a share live on the owner's storage citizens. A reader
  fetches them through an ARC call to the owner's storage, or through the
  owner's devices. This is open. A storage citizen and a relay never hold a
  share key.

### 5.2 Phase B: cloning

A reader copies the entries that they can read into their own journal. The
copy records its origin: owner, address, entry and time. It is no longer
linked. A clone needs nothing that the reader does not have already.

### 5.3 Phase C: search, open items, timeline, mentions

- The search index stays on the device and covers shared entries as they
  arrive.
- `journal todo` lists each `- [ ]` that no later `- [x]` or later entry
  closes.
- `journal log --since 2d` merges the entries of own and shared notebooks
  by time, with `--notebook`, `--owner` and `--author`.
- A mention sends a short mail to the citizen that it names. It holds the
  link, not the text.

### 5.4 Phase D: devices, attachments, templates, export

- Several devices: entries do not conflict, so phase A covers it. The open
  question is policy: does a device key of the device list sign entries, as
  it signs drafts today?
- Time of receipt: a storage citizen adds a signed time when it stores a
  blob. That changes the storage protocol ([storage](../storage/SPEC.md)).
  A reader shows a warning when the write time and the time of receipt are
  more than one hour apart (*estimated* threshold).
- Attachments: a file of the files app, linked from an entry, sealed so
  that a reader of the entry can open it.
- Templates: fixed shapes in the manifest, used with `--template`.
- Export: Markdown or HTML, with each `journal+arc://` link rewritten to a
  local file.

## 6. Prerequisite: deleted pages leave the storage citizens

A delete that works leaves two records to upload: the blank draft and the
kind 5 request ([interface](../interface/SPEC.md), section 3.10;
[storage](../storage/SPEC.md), measured at 3 blobs for a deleted page). So
132 records after a delete is not the path of a delete that worked.

The likely cause, not yet proven, is a write that failed half way:

1. A publish saves its events one by one: the parts, then the draft, then
   the checkpoint (`runtime/iface/data.go:386`,
   `runtime/citizen/environment.go:124-134`).
2. Cato's page of 4.04 MB failed on the draft, with the codec limit of
   section 0. Its parts were in the store already, and no draft names
   them.
3. `arc sync` uploads every part of the citizen
   (`runtime/selfsync/sync.go:23` and `:35-54`). A page of 4.04 MB has about
   124 parts of 32 KiB (*arithmetic*: 4,040,000 / 32,768), close to the 132
   records and the 3.9 MiB that each storage citizen gained (*measured*).
4. `journal delete` of a page with no draft stops with
   `there is nothing to delete` (`runtime/iface/data.go:469`), and deletes no
   part.
5. Repair deletes a blob only when a later record that is on storage
   replaces or deletes it (`runtime/selfsync/repair.go:223-224`, `:509-517`).
   No record names the orphan parts, so repair keeps them.

Cato's steps say that he deleted the page. The trace cannot yet tell
whether his delete found a draft. The full output of his `arc sync`, or a
count of the parts of that address in his store, decides it. A second
reading is possible: a sync that cannot open the piece tags of an older
generation codes those blobs again (`runtime/selfsync/network.go:311-317`).
It prints `refused <n> pieces of generation <g>`.

PR A0 fixes the delete path:

- A publish checks the size of the draft before it saves any part, and
  says `the page is <n> MB; the largest page is <m> MB` (Cato's finding 3).
- A failed publish deletes the parts that it saved.
- `journal delete` deletes each part and checkpoint of the address, also
  with no draft.
- Repair deletes a part or a checkpoint whose draft is blank or missing on
  every storage citizen and on the device.
- Tests: a write that fails on size leaves no part in the store; a delete
  of a page of many parts leaves no event of kind 3275 or 1234; a sync after
  both puts only the blank draft and the request; repair frees an orphan
  part. Each test fails on `16c08bb`.

After A0 is deployed, a repair on Cato's home frees his 4 MiB.

## 7. Compatibility

- An older arc refuses the new journal manifest at its next sync (section
  0). After its update, it reads the journal again.
- An older arc cannot open an entry. A newer arc reads each v1 page.
- An older storage app keeps the blobs of entries, as it keeps drafts
  today. The time of receipt of phase D needs an update of the storage app.
- An older reader cannot open a share. Phase B says how a reader learns
  that it needs an update.

## 8. Upgrade order

1. Build PR A0, then A1. Otto and Cato test each one as users.
2. Release. The release carries the runtime, not the announcement.
3. Update every home that uses the journal: Zim's Mac, Otto's and Cato's
   homes, and each agent. The relays and the storage citizens gain nothing
   from A1, so they need no deploy for it.
4. Only then, announce the new manifest from Zim's key. An older home that
   was missed loses `arc journal` until it updates.

## 9. Decisions for Zim

These touch keys or protocols, so Zim decides them directly:

1. Share keys: one per share, sealed to a reader, rotated on revoke
   (section 5.1).
2. Share links that carry a key, and the revoke of one link.
3. Redact: a signature of the owner, and its reach into shares (section
   4.4).
4. Attachment keys for readers (section 5.4).
5. Signing of entries by device keys (section 5.4).
6. A time of receipt signed by storage citizens (section 5.4).
7. The inner kind of an entry (section 4.1).

## 10. Proof

Nothing is built. Each phase names its tests in its build PR.
