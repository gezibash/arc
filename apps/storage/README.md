# Storage app

The storage app holds blobs for the owners of a storage citizen. A storage
citizen is a citizen that serves this app. The first storage citizen is a
machine of the user, such as a Mac mini at home. There is no billing. Each
account has a quota, a plan name and an end time. The plan and the end keep
a place for a paid service later.

ARC carries one call for each session: a WebRTC offer, signed by the account
key of the owner or by a device key with a grant. The bytes go on the direct pipe of `sdk/direct`, not through
the relay. If no direct path opens, the bytes go through the TURN server of
the relay, as the last resort, and the command says so.

This app is step 1 of [the storage proposal](../../docs/proposals/storage.md)
(issue #198). The proposal gives the full design: device keys, grants,
wraps, reconciliation and the sync of the journal.

| Part | Status |
| --- | --- |
| The service: put, list from a cursor, get, delete and usage | Built. Tested with the built `arc`, a local relay, a storage citizen, two arc homes of one owner and a stranger. |
| Admission by a join call: the rules `invite`, `ask` and `open` | Built. Tested through `arc keys storage add` and the commands of the operator. |
| Requests in flight, lists of IDs in `get` and `delete`, and `hello` | Built. Tested: the client keeps at most 32 puts in flight, each reply goes to its own request, the bytes of each put keep their order, and the app ends a pipe with more puts in flight. On an emulated link of 44 ms, 1,000 puts of 4 KiB took 117.1 to 121.3 s before and 7.9 to 8.5 s after (measured). |
| The index of each account in bbolt | Built. Tested: a stop between the bytes and the entry leaves a consistent index, and the app imports an older `state.json` once. |
| The quota, plan, end and state of each account | Built. Tested: a put over the quota of one account fails while another account has room. |
| The usage line of `arc sync` | Not built. `arc-storage usage` prints the usage. |
| The removal of the bytes of a closed account | Not built. The operator removes the directory of the account. |
| The account key of the owner | Built. Tested: the two homes of the owner use one account, named by the account key, not by the key of the owner. |
| Grant sets for device keys, and revocation | Built. Tested with a device behind a bunker, a revoked device, and a second storage citizen. |
| The owner check, by the key of the caller | Built. Tested. |
| Two machines on different networks | Not tested. The transfer app tested the same pipe by hand; see its README. |
| Encryption of the blobs | Not in this app. `arc sync` encrypts each blob of self data. `arc-storage put` does not encrypt. See "What the storage citizen sees". |
| The sync of the journal and the other self data | Built in `arc sync`, step 2 of the proposal. See [the storage spec](../../docs/storage/SPEC.md). |
| The storage list of kind 10273 | Not built. A later step of the proposal. |

The ARC call and the messages on the pipe are in the package `sdk/storage`, so
`arc` uses them too. [The storage spec](../../docs/storage/SPEC.md) defines
them.

## How it works

1. The operator of the storage citizen serves the app, and chooses a rule
   of admission. The default rule is `invite`: the operator makes an invite
   link, and gives it to a citizen.
2. The citizen runs `arc keys storage add <link>` on its signer. The account
   key sends one join call. The app admits the account at the quota of the
   invite. The signer installs the app of the storage citizen.
3. An owner installs the app of the storage citizen one time on each
   machine. On the signer, `arc keys devices add` sends the grant set: the
   device keys that can use the account.
4. `arc-storage` makes a WebRTC offer. It runs the manifest command `open`
   of the installed app, which sends one ARC call. On the signer, ARC signs
   the call with the account key. On a device behind a bunker, ARC signs it
   with the device key.
5. The app finds the account of the caller: an admitted account key, or a
   device key in the newest grant set of an admitted account. It replies
   with a WebRTC answer.
6. The two programs open one data channel. `arc-storage` sends its
   requests on it, at most 32 in flight.

## The account key and the grants

The storage citizen knows an account by an account key. The signer derives
it from the citizen key for one storage citizen:
`HKDF-SHA256(citizen secret key, info = "arc-storage-account-v1" || key of the storage citizen)`.
[The storage spec](../../docs/storage/SPEC.md), section 3.1, defines it.

- Each machine with the citizen key derives the same account key for one
  storage citizen, and uses one account.
- Two storage citizens get two account keys. They cannot join them.
- The storage citizen never sees the citizen key.
- A device behind a bunker cannot derive the account key. It signs with its
  device key, and the reply is sealed to its device key. The bunker keeps its
  default `--decrypt self`.

The account key sends the grant set: each device key of the device list.
The signer sends it after `arc keys devices add` and `revoke`, and on each
`arc sync`. The newest set replaces the older set. A device key that the
newest set does not hold gets `not_owner`. The signer sends a set only to
the storage citizens that it installed.

## Use it

Build with `mise run build`, and use `bin/arc` and `bin/arc-storage`.

### The storage citizen

Make a citizen with its own key on the machine that stays on. Give it a
relay. Then serve the app:

```sh
arc serve apps/storage
```

`arc serve` prints the key of the storage citizen. Make an invite link for
each citizen that may use it, and give the link to the citizen as text or a
QR code:

```sh
arc-storage invite -quota 1GiB -plan friend
```

```text
storage+arc://3b6a…?invite=9f2c…
```

The link admits one account, and is good for 7 days.

`allow` in the `Arcfile` matches the key of the caller. The caller is an
account key or a device key, which no one can join to a citizen key. Thus
`allow` cannot name friends. The invite token carries the consent. If the
`Arcfile` has `allow`, `arc serve` refuses the join of each new account key
before the app sees it, and the signer gets an error of the call, not
`refused`. Remove `allow` to admit accounts by a join.

### The rules of admission

`STORAGE_ADMIT` chooses the rule for a join with no invite token. A good
token admits under each rule.

| Rule | A join with no token |
| --- | --- |
| `invite` (the default) | Refused: `this storage citizen admits only with an invite`. |
| `ask` | Pending. The app keeps the request for 30 days. The operator decides. |
| `open` | Admitted at `STORAGE_OPEN_QUOTA`, until the sum of the quotas passes `STORAGE_CAPACITY`. The sum counts each account of `STORAGE_OWNERS` at `STORAGE_QUOTA`. |

Under `ask`, the operator lists the requests, and admits or refuses each:

```sh
arc-storage requests
arc-storage accept <account key> -quota 1GiB
arc-storage refuse <account key>
```

`requests` prints one line for each request: the account key, the bytes
that it wants, the time, the citizen key or `-`, and the note. A join shows
the citizen key only if the citizen runs `arc keys storage add --show-key`.

`accept` can also admit an account key that the citizen gave by hand, with
`arc keys account <key of the storage citizen>`. `STORAGE_OWNERS` still
admits account keys at `STORAGE_QUOTA`, as before.

### The accounts

```sh
arc-storage account set <account key> -quota 2GiB -plan friend -expires 2027-01-31
arc-storage account close <account key>
```

- A lower quota than the bytes in use stops each put. Reads and deletes
  work.
- `close`, or the day of `-expires`, makes the account `closing`: it refuses
  each put, and keeps reads, lists and deletes for 30 days. Then it is
  `closed`, and takes no session.

The commands of the operator change the files in `STORAGE_DIR` under the
locks of the app. They run while the app runs, and each change counts at
the next call. Each takes `-dir`; the default is `STORAGE_DIR`.

### The owner

On the signer, ask for an account with the invite link:

```sh
arc keys storage add 'storage+arc://<key>?invite=<token>'
```

```text
storage: admitted: 0 B of 1 GiB in use, plan friend, open
installed storage
```

Without a link, give the key of the storage citizen. Under the rule `ask`,
the reply is `pending`; run the command again after the operator decides.
`--want 2GiB` and `--note <text>` go to the operator. `--as <name>` names
the install.

Install the app of the storage citizen one time on each other machine of
the owner:

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

Delete a blob, and show the bytes in use, the quota, the plan and the state
of the account:

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
| `STORAGE_OWNERS` | Account keys, in hex, that the app admits with no join, separated by spaces or commas. `arc keys account <key of the storage citizen>` prints the account key of a citizen. |
| `STORAGE_QUOTA` | The quota of each account of `STORAGE_OWNERS`, such as `65536`, `64KiB`, `512MiB` or `1GiB`. The default is `1GiB`. |
| `STORAGE_ADMIT` | The rule of admission: `invite` (the default), `ask` or `open`. |
| `STORAGE_OPEN_QUOTA` | The quota of an account under `open`. The default is `STORAGE_QUOTA`. |
| `STORAGE_CAPACITY` | The largest sum of the quotas under `open`. `open` needs it. |
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

- The app takes a session from an admitted account key, or from a device
  key in the newest grant set of an admitted account. It refuses each other
  caller, and each caller of a closed account, with `not_owner`. No pipe
  opens.
- Only an admitted account key sets the grant set of its account. A set with an older `at` than the set of the account gets
  `stale_grants`.
- A blob has a size that is a power of two, from 4 KiB to 64 MiB. The app
  refuses another size with `bad_size`.
- The quota of the account counts the sizes of its blobs. A put that passes
  the quota gets `over_quota`, with the bytes in use, the quota and the size of the blob:
  `over quota: the account uses 4096 of 65536 bytes, and this blob needs 65536 more`.
- A put of an ID that the account holds gets `held`. No bytes go.
- A delete removes the bytes at once, and keeps the ID as a tombstone. For
  90 days, a put of that ID gets `tombstoned`. No bytes go.
- Each put and each delete gets the next sequence number of the account. A
  list gives the last change of each ID after the cursor, in order, at most
  1000 for each request. `arc-storage list` asks until the end.
- The app holds at most 4 pipes at a time. A pipe that carries no request
  for 30 s closes.
- The app answers the requests of a pipe in order. A client keeps at most
  32 puts in flight. If a client sends a 33rd put, the app ends the pipe.

## Storage on disk

```text
<STORAGE_DIR>/invites.json                        the SHA-256 of each invite token, its quota, plan and end
<STORAGE_DIR>/accounts/<account key>/state.json   the record and the grant set
<STORAGE_DIR>/accounts/<account key>/index.db     the index: the cursor, the bytes in use and the entries
<STORAGE_DIR>/accounts/<account key>/blobs/<id>   the bytes of one blob
```

`state.json` and `invites.json` are written to a temporary file, flushed,
and renamed (`sdk/atomicfile`). A lock on each account covers each change
of `state.json`. A lock on `invites.json` covers each join, and then the
lock of the account. A join spends the token first, and then admits the
account: if the program stops between the two, the token is lost, and no
token admits twice.

`index.db` is a [bbolt](https://github.com/etcd-io/bbolt) file with pages
of 4 KiB. Only the service opens it; the commands of the operator change
only `state.json` and `invites.json`. A put or a delete changes a few keys
in one transaction, and does not write the whole index. With 10,000 blobs
in one account, a put wrote about 31 KB of index pages, and the last 1,000
of 10,000 puts wrote 1.35 times the bytes of the first 1,000 (measured).
Before, each put wrote the whole `state.json`, about 132 bytes for each
entry: 1.3 MB for each put at 10,000 entries (computed from the measured
entry size).

A put writes the blob first, and then the entry. The app writes the file
of each put when its bytes are complete. It syncs the directory and writes
the entries of a group of puts in one transaction, and only then answers
`stored`. A delete writes the entry first, and then removes the blob. If
the program stops between the two, a file with no entry stays. The app
does not serve it, and the next put of that ID replaces it.

### The import of an older state.json

Before `index.db`, `state.json` also held the cursor, the bytes in use and
the entries. At its start, the service opens the index of each account.
If `state.json` holds entries, the service imports them into `index.db` in
one transaction, with a mark, and then writes `state.json` without them.
The lock of the account covers the import.

- If the program stops before the transaction ends, `index.db` holds
  nothing of the import, and the next start imports again.
- If the program stops after the transaction and before `state.json` is
  written, the next start sees the mark. It does not import again. It only
  writes `state.json`.
- A command of the operator that runs before the import keeps the entries
  in `state.json`.
- The sequence numbers do not change, so the cursor of each device stays
  good.
- An older storage app cannot read `index.db`. After the import, it sees
  each account as empty. Do not go back to an older storage app.

## The blob format of `arc-storage`

`put` makes a blob from a file: the length of the file in 8 bytes, big
endian, then the bytes of the file, then zeros up to the next power of two,
at least 4 KiB. The ID is the SHA-256 of the blob. `get` checks the SHA-256,
and removes the padding.

`arc sync` makes another blob, as section 3.3 of the storage spec says: its
ID is a keyed hash of the record ID, and the padding is inside the
encryption. The app does not read the blob, so the app does not change for
it.

## What the storage citizen sees

- `arc-storage` does not encrypt. The storage citizen can read a file that
  the owner did not encrypt before the put. The SHA-256 of a blob also
  names a file that the storage citizen knows. Encrypt each file before the
  put. `arc sync` encrypts each record of self data.
- The storage citizen sees the account key and the device keys of the
  grant set. It does not see the citizen key, the device list, or the names
  of the devices.
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
{"v":1,"session":{"hold_ms":0,"stun":"","sdp":{"type":"offer","sdp":"..."}}}
```

```json
{"v":1,"sdp":{"type":"answer","sdp":"..."}}
```

`hold_ms` and `stun` have the meaning of the transfer app. See the
[direct pipe spec](../../docs/direct/SPEC.md), sections 3.5 and 3.6.

| Error | Cause |
| --- | --- |
| `invalid_request` | The body is not a request of version 1. |
| `not_owner` | The caller is not an account key in `STORAGE_OWNERS`, and no grant set holds it. |
| `busy` | The app holds 4 pipes. |
| `storage_failed` | The app could not read its disk, or open its end. |

The signer sends the grant set in another call, which the account key
signs. `at` is the time of the device list event:

```json
{"v":1,"grants":["<device key>","<device key>"],"at":1791800000}
```

```json
{"v":1,"devices":2}
```

The errors are `invalid_request`, `not_owner` (the author is not an
admitted account), `stale_grants` and `storage_failed`.

The signer asks for an account in a third call, which the account key
signs. Each field of `join` is optional. `citizen` and `sig` show the
citizen key, by choice; the storage spec, section 3.2, defines the
signature.

```json
{"v":1,"join":{"invite":"9f2c…","want":1073741824,"note":"Ana from the choir"}}
```

```json
{"v":1,"account":{"status":"admitted","quota":1073741824,"used":0,"plan":"friend","state":"open","tombstone_days":90,"read_days_after_close":30}}
{"v":1,"account":{"status":"pending","message":"the operator decides by hand; ask again later"}}
{"v":1,"error":"refused","message":"this storage citizen admits only with an invite"}
```

A refusal is a reply body with a message, not an error of the call. An
older storage app answers the join with `invalid_request`.

### The pipe

`arc-storage` opens one data channel. Each request is one text message of
JSON. Each reply is one text message of JSON. The app answers the requests
in order.

| Request | Reply |
| --- | --- |
| `{"op":"hello"}` | `{"v":2,"features":["batch"],"window":32}`. An older app answers `invalid_request`. |
| `{"op":"put","id":"<64 hex>","size":4096}` | `{"status":"send"}`, then the client sends the bytes, and the app answers `{"status":"stored","seq":7}`. Or `{"status":"held"}` or `{"status":"tombstoned"}`, and no bytes go. |
| `{"op":"list","after":6,"limit":1000}` | `{"entries":[{"seq":7,"id":"…","size":4096},{"seq":8,"id":"…","deleted":true}],"next":8}` |
| `{"op":"get","id":"<64 hex>"}` | `{"size":4096}`, then the bytes of the blob. |
| `{"op":"get","ids":["<64 hex>",…]}` | `{"items":[{"id":"…","size":4096},{"id":"…","error":"not_found"}]}`, then the bytes of each found blob, in order. |
| `{"op":"delete","id":"<64 hex>"}` | `{"status":"deleted","seq":9}`, or `{"status":"absent"}`. |
| `{"op":"delete","ids":["<64 hex>",…]}` | `{"items":[{"id":"…","status":"deleted","seq":9},{"id":"…","status":"absent"}]}` |
| `{"op":"usage"}` | `{"used":8192,"quota":1073741824,"blobs":2,"state":"open","plan":"free"}` |

- The client sends `hello` one time in each session, before its first
  request in flight. If `hello` gives the feature `batch`, the client numbers each request
  with `n`, from 1. Each reply to the request carries the same `n`. The
  client keeps at most `window` puts in flight: sent, with no `stored`,
  `held`, `tombstoned` or error yet.
- The client sends the bytes of each put after its `send` reply, in the
  order of the `send` replies. The app gives each run of bytes to the
  oldest put that got `send` and has not all its bytes. A text request can
  come between two runs.
- A request with `ids` names 1 to 256 IDs.
- Without `batch`, the client sends one request at a time, with no `n`, as
  before.

The bytes of a blob go in binary messages of 16 KiB, with the flow control
of the direct pipe spec, section 3.4. A refused request gets
`{"error":"<code>","message":"<text>"}`, with the `n` of the request. The
codes are `invalid_request`, `bad_size`, `over_quota`, `refused` (a put on
a closing account), `not_found` and `storage_failed`.

### Measure the puts

`TestEmulatedLinkPuts` puts blobs through the app and the client on a link
that pion emulates, as `TestEmulatedLinkSpeed` of `sdk/direct` does. It
prints the time, and does not check it:

```sh
STORAGE_LINK_PUTS=1000 DIRECT_LINK_RTT_MS=44 go test ./apps/storage/server -run TestEmulatedLinkPuts -count=1 -v
```

Results on an Apple Silicon Mac, 2026-10-12, link of 92.5 Mbit/s, 44 ms and
a queue of 128 KiB, 1,000 puts of 4 KiB:

| Code | Time |
| --- | --- |
| One request at a time, `state.json` (before) | 117.1 to 121.3 s, three runs |
| 32 puts in flight, `index.db` (after) | 7.9 to 8.5 s, four runs |

After, the disk takes most of the time: each blob file gets its own sync,
and one sync costs about 4.9 ms on this Mac (measured).

## Later steps

- **The storage list.** Kind 10273 names the storage citizens of a
  citizen, so that each device installs them. Until then, each device runs
  `arc install`.
- **A paid service.** An `offer` in a refused or pending reply, and a `pay`
  field in the join. `plan` and `expires` keep the place.

## Tests

```sh
go test ./apps/storage/...
```

The tests build `arc` and `arc-storage`, and run them as a person does: a
relay on this machine, a storage citizen with `arc serve`, two arc homes
with the key of one owner, and a stranger. They check these things:

- One home puts blobs. The second home lists from a cursor, gets only the
  new blob, and gets bytes that are equal to the bytes put.
- The two homes use one account, named by the account key, and each blob on
  disk has a padded size.
- A delete reaches the second home from its cursor. A put of the deleted ID
  gets `tombstoned`.
- A put over the quota fails with the bytes in use and the quota.
- A stranger gets `not_owner`, and no pipe opens.
- Under `invite`, a join with no token, with a wrong token, and with a
  spent token is refused. A join with a token is admitted at its quota and
  plan.
- Under `ask`, a join is pending. `requests` shows it with the citizen key
  and the note. `accept` admits it, and `refuse` refuses it.
- Under `open`, joins are admitted at the open quota until the capacity.
  A put over the quota of one account fails while another account has
  room. `usage` reports the quota, plan and state of each account.
  `account set` raises a quota, and `account close` refuses puts and keeps
  lists.
- A newer grant set revokes a device key, and an older set cannot bring it
  back (`go test ./apps/storage/server/`).

The proof `TestStorageJournalSync` in `internal/proof` runs a device behind a
bunker, a revoked device, and a second storage citizen.

Each test failed with a temporary break of the behavior that it tests.
