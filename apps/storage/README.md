# Storage app

The storage app holds blobs for the owners of a storage citizen. A storage
citizen is a citizen that serves this app. The first storage citizen is a
machine of the user, such as a Mac mini at home. There is no billing.

ARC carries one call for each session: the pseudonym of the owner and a
WebRTC offer. The bytes go on the direct pipe of `sdk/direct`, not through
the relay. If no direct path opens, the bytes go through the TURN server of
the relay, as the last resort, and the command says so.

This app is step 1 of [the storage proposal](../../docs/proposals/storage.md)
(issue #198). The proposal gives the full design: device keys, grants,
wraps, reconciliation and the sync of the journal.

| Part | Status |
| --- | --- |
| The service: put, list from a cursor, get, delete and usage | Built. Tested with the built `arc`, a local relay, a storage citizen, two arc homes of one owner and a stranger. |
| The quota of each account | Built. Tested. |
| The pseudonym of the owner | Built. Tested on one storage citizen: the two homes of the owner use one account, and the name of the account is not the key of the owner. |
| The owner check, by the key of the caller | Built. Tested. |
| Two machines on different networks | Not tested. The transfer app tested the same pipe by hand; see its README. |
| Encryption of the blobs | Not in this app. The client must encrypt each blob before the put. See "What the storage citizen sees". |
| Device keys, grants that the account key signs, invites | Not built. See "Later steps". |
| The sync of the journal, the storage list of kind 10273 | Not built. Later steps of the proposal. |

## How it works

1. The operator of the storage citizen serves the app, and names its owners
   and the quota.
2. An owner installs the app of the storage citizen one time.
3. `arc-storage` makes a WebRTC offer. It runs the manifest command `open`
   of the installed app. The command adds the pseudonym of the owner, and
   sends one ARC call. ARC signs the call with the key of the owner.
4. The app checks that the caller is an owner, and that the caller has the
   grant of the account. It replies with a WebRTC answer.
5. The two programs open one data channel. `arc-storage` sends its
   requests on it, one at a time.

## The pseudonym of the owner

The storage citizen knows an account by a pseudonym. The manifest command
`open` makes the pseudonym with the template `{{author|keyed:account}}`.
That is a keyed value of section 3.7.1 of
[the interface spec](../../docs/interface/SPEC.md). The keyed root comes
from the secret key of the owner, and the key of the storage citizen goes
into the key of the value. Thus:

- Each machine with the key of the owner makes the same pseudonym for one
  storage citizen.
- Two storage citizens get two pseudonyms. They cannot join them.
- A pseudonym alone is not a credential. The first key that opens an
  account gets its grant. The app refuses another key for that account
  with `not_owner`.

Before device keys exist, the owner signs the ARC call with its citizen key.
The storage citizen therefore sees the citizen key, and keeps it as the
grant of the account. The pseudonym starts to hide the citizen key only when
a device key signs the call (see "Later steps").

## Use it

Build with `mise run build`, and use `bin/arc` and `bin/arc-storage`.

### The storage citizen

Make a citizen with its own key on the machine that stays on. Give it a
relay. Then serve the app with the keys of its owners and the quota:

```sh
STORAGE_OWNERS=<key of the owner> STORAGE_QUOTA=1GiB arc serve apps/storage
```

`arc serve` prints the key of the storage citizen. To keep other citizens
from the app, also set `allow` in the `Arcfile` to the keys of the owners.
Then `arc serve` refuses them before the program sees them.

### The owner

Install the app of the storage citizen one time on each machine of the
owner:

```sh
arc install <key of the storage citizen> storage
```

Give files. `put` pads each file to a blob, and prints one line for each:
the ID, the status and the file.

```sh
arc-storage put page-1.enc page-2.enc
```

```text
3f1c…	stored	page-1.enc
9a07…	stored	page-2.enc
```

List the changes after a cursor. Each line has the sequence number, the ID,
and the size of the blob or `deleted`. `list` prints the next cursor on
standard error. The first list uses the cursor 0.

```sh
arc-storage list -after 0
```

```text
1	3f1c…	4096
2	9a07…	8192
```

On another machine of the owner, list from the last cursor of that machine,
and get the new blobs. `get` writes each file with the name of its ID, in
the directory of `-dir`:

```sh
arc-storage list -after 2
arc-storage get -dir pages 3f1c… 9a07…
```

Delete a blob, and show the bytes in use:

```sh
arc-storage delete 3f1c…
arc-storage usage
```

## Settings

The service reads its settings from the environment. `arc serve` gives its
environment to the program.

| Variable | Value |
| --- | --- |
| `STORAGE_DIR` | The directory of the blobs. The default is `~/.local/state/arc-storage`. |
| `STORAGE_OWNERS` | The public keys, in hex, that can open an account, separated by spaces or commas. If it is not set, the app refuses each caller. |
| `STORAGE_QUOTA` | The quota of each account, such as `65536`, `64KiB`, `512MiB` or `1GiB`. The default is `1GiB`. |
| `STORAGE_STUN` | The STUN server, or `none`. If it is not set, the service asks the first relay of `ARC_RELAYS` on port 3478. The commands read it too. |
| `STORAGE_LOOPBACK` | If set, an end also uses the loopback address. The tests set it, because their two ends are on one machine. The commands read it too. |

Each command takes these flags:

| Flag | Value |
| --- | --- |
| `-arc <program>` | The arc program. The default is `arc` on `PATH`. |
| `-home <directory>` | The arc home. The default is the home that arc picks. |
| `-app <name>` | The installed name of the storage app. The default is `storage`. Use `arc install --as` and this flag for a second storage citizen. |
| `-stun <url>` | The STUN server, or `none`. The default is `STORAGE_STUN`, or the first relay of `arc relay ls` on port 3478. |
| `-hold` | Ask the storage citizen to wait in the first attempt, not in the second. |
| `-no-relay` | Do not try the TURN server of the relay. If no direct path opens, the command fails. |

`list` takes `-after <cursor>`. `get` takes `-dir <directory>`, or
`-o <file>` for one ID.

## Rules of the storage citizen

- The app refuses a caller that is not in `STORAGE_OWNERS` with
  `not_owner`. No pipe opens.
- The first owner key that opens an account gets its grant. The app refuses
  another key for that account with `not_owner`.
- A blob has a size that is a power of two, from 4 KiB to 64 MiB. The app
  refuses another size with `bad_size`.
- The quota counts the sizes of the blobs. A put that passes the quota gets
  `over_quota`, with the bytes in use, the quota and the size of the blob:
  `over quota: the account uses 4096 of 65536 bytes, and this blob needs 65536 more`.
- A put of an ID that the account holds gets `held`. No bytes go.
- A delete removes the bytes at once, and keeps the ID as a tombstone. For
  90 days, a put of that ID gets `tombstoned`. No bytes go.
- Each put and each delete gets the next sequence number of the account. A
  list gives the last change of each ID after the cursor, in order, at most
  1000 for each request. `arc-storage list` asks until the end.
- The app holds at most 4 pipes at a time. A pipe that carries no request
  for 30 s closes.

## Storage on disk

```text
<STORAGE_DIR>/accounts/<pseudonym>/state.json   the grant, the cursor, the entries
<STORAGE_DIR>/accounts/<pseudonym>/blobs/<id>   the bytes of one blob
```

Each file is written to a temporary file, flushed, and renamed
(`sdk/atomicfile`). A lock on each account covers each change of
`state.json`. A put writes the blob first, and then the state. A delete
writes the state first, and then removes the blob. If the program stops
between the two, a file with no entry stays. The app does not serve it,
and the next put of that ID replaces it.

## The blob format of `arc-storage`

`put` makes a blob from a file: the length of the file in 8 bytes, big
endian, then the bytes of the file, then zeros up to the next power of two,
at least 4 KiB. The ID is the SHA-256 of the blob. `get` checks the SHA-256,
and removes the padding.

The sync client of a later step makes another blob, as section 2.4 of the
proposal says: its ID is a keyed hash of the record ID, and the padding is
inside the encryption. The app does not read the blob, so the app does not
change for it.

## What the storage citizen sees

- `arc-storage` does not encrypt. The storage citizen can read a file that
  the owner did not encrypt before the put. The SHA-256 of a blob also
  names a file that the storage citizen knows. Encrypt each file before the
  put, until the sync client of a later step encrypts the records.
- The storage citizen sees the citizen key of the owner, because the owner
  signs the ARC call with it (see "The pseudonym of the owner").
- It sees the count of the blobs, their padded sizes, and the times of the
  puts and the sessions.
- With a direct path, it sees the IP address of the owner. With the TURN
  server, the relay sees the IP addresses of both machines.

## Request protocol

### The ARC call

A request is a live call to the capability `storage`, with the method
`RAW` and the path `/`. The manifest command `open` makes the body from the
session that `arc-storage` gives:

```json
{"v":1,"account":"<pseudonym>","session":{"hold_ms":0,"stun":"","sdp":{"type":"offer","sdp":"..."}}}
```

```json
{"v":1,"sdp":{"type":"answer","sdp":"..."}}
```

`hold_ms` and `stun` have the meaning of the transfer app. See the
[direct pipe spec](../../docs/direct/SPEC.md), sections 3.5 and 3.6.

| Error | Cause |
| --- | --- |
| `invalid_request` | The body is not a request of version 1. |
| `not_owner` | The caller is not in `STORAGE_OWNERS`, or has no grant in the account. |
| `busy` | The app holds 4 pipes. |
| `storage_failed` | The app could not read its disk, or open its end. |

### The pipe

`arc-storage` opens one data channel. Each request is one text message of
JSON. The app answers each request with one text message of JSON before
the next request.

| Request | Reply |
| --- | --- |
| `{"op":"put","id":"<64 hex>","size":4096}` | `{"status":"send"}`, then the client sends the bytes, and the app answers `{"status":"stored","seq":7}`. Or `{"status":"held"}` or `{"status":"tombstoned"}`, and no bytes go. |
| `{"op":"list","after":6,"limit":1000}` | `{"entries":[{"seq":7,"id":"…","size":4096},{"seq":8,"id":"…","deleted":true}],"next":8}` |
| `{"op":"get","id":"<64 hex>"}` | `{"size":4096}`, then the bytes of the blob. |
| `{"op":"delete","id":"<64 hex>"}` | `{"status":"deleted","seq":9}`, or `{"status":"absent"}`. |
| `{"op":"usage"}` | `{"used":8192,"quota":1073741824,"blobs":2}` |

The bytes of a blob go in binary messages of 16 KiB, with the flow control
of the direct pipe spec, section 3.4. A refused request gets
`{"error":"<code>","message":"<text>"}`. The codes are `invalid_request`,
`bad_size`, `over_quota`, `not_found` and `storage_failed`.

## Later steps

- **Device keys.** Base step 1 (#197) gives each device its own key. The
  account key, which the signer derives for each storage citizen, then
  signs a grant for each device key. The owner check of the app goes in
  `HandleRequest` of `server/server.go`, and the grants in
  `store.authorize` of `server/store.go`. Those two places change, and the
  messages on the pipe stay.
- **Invites.** `arc-storage invite --quota 1GiB` replaces
  `STORAGE_OWNERS`, as section 2.2 of the proposal says.
- **Sync.** A transport adapter encrypts the records of the local store,
  and syncs them with this app.

## Tests

```sh
go test ./apps/storage/...
```

The tests build `arc` and `arc-storage`, and run them as a person does: a
relay on this machine, a storage citizen with `arc serve`, two arc homes
with the key of one owner, and a stranger. They check these things:

- One home puts blobs. The second home lists from a cursor, gets only the
  new blob, and gets bytes that are equal to the bytes put.
- The two homes use one account, named by a pseudonym, and each blob on
  disk has a padded size.
- A delete reaches the second home from its cursor. A put of the deleted ID
  gets `tombstoned`.
- A put over the quota fails with the bytes in use and the quota.
- A stranger gets `not_owner`, and no pipe opens.
- A second key cannot use the account of another key.

Each test failed with a temporary break of the behavior that it tests.
