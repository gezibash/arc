# Changelog

All notable changes to ARC are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed

- **Breaking:** the default home of `arc` is `~/.arc`. A home that an older
  arc made in `~/.config/arc` stays in use while `~/.arc` holds no entry of
  arc (`citizens`, `default`, `device`, `relay.db`, `wake`, `wake.toml`), so
  an installed machine and a relay volume keep working. arc moves no file.
  `--home` and `ARC_HOME` still win. arc never takes a home from the working
  directory. A new install needs this arc or a newer one. An older arc looks
  only in `~/.config/arc`, so it does not find a home in `~/.arc` unless it
  gets `--home ~/.arc` or `ARC_HOME=~/.arc`. The Docker image declares its
  volume at `/home/arc/.arc`.
- arc-dm resolves no home of its own. It runs the arc program, passes
  `--home` only when it got one, and keeps its state in
  `~/.local/state/arc-dm`. So it follows the arc that it runs: a newer
  arc-dm with an older arc does not see a home in `~/.arc`. Upgrade arc and
  arc-dm together; they ship in one tarball.
- **Upgrade first:** upgrade each arc and arc-dm on a machine before you use
  any of them, or set `ARC_HOME`. An older arc does not see `~/.arc`. When
  it finds no home in `~/.config/arc`, it tells you to run `arc keys gen`,
  and that makes a second identity there.
- If `~/.arc` and `~/.config/arc` both hold an identity, each command warns
  once and names both folders. arc uses `~/.arc`.
- If arc cannot read `~/.arc` or `~/.config/arc`, for example after
  `sudo arc`, each command fails with an error that names the folder. arc
  never uses the identity of the other folder in its place, and never makes
  a new one. A `default` file that arc cannot read is an error too, not a
  missing identity, and `arc keys gen`, `recover` and `add` stop before they
  write a key.
- `arc install` refuses the name of an entry of the arc home as the name of
  an app.
- **Breaking:** an open or a call that no provider hears is not submitted,
  and arc tries it again for at most 6 s (docs/sessions/SPEC.md,
  docs/delivery/SPEC.md). It tries again only when no one listened, so a
  request that went out is never submitted twice. An older caller still
  fails at once (#436).
  - A call whose deadline ends while no provider hears it says so and names
    the relay of the provider, not the deadline of the caller (#453).
  - arc does not start a try that cannot end before the deadline of the
    caller, so the deadline never cuts off a try after its request went
    out.

## [0.37.0] - 2026-10-19

Mail follows a recipient who moves to other relays, and waiting mail goes
to relays that are learned later. A sync goes to TURN first for 6 hours
after the direct attempts to a storage citizen find no path. A key with a
letter that is not hex is refused on amd64 too. Found and tested by Otto
and Cato. Nothing is breaking. Update each relay, then each device.

### Changed

- Each send asks the relays and indexers for the NIP-17 relay list of the
  recipient, for at most 5 s, and uses the newest. If none answers, it uses
  the stored list and says so. With no list, it says that the mail waits on
  the sender's relays only. After the relays, `arc sync` and each
  `arc serve` tick send waiting messages and requests to relays of the
  current list that lack their wraps (docs/delivery/SPEC.md, section 3.8).
  An older arc looks in its store first, so it keeps using a moved
  recipient's old relays (#374).
- After the direct attempts to a storage citizen find no path, a sync that
  takes a pipe through TURN goes to TURN first for that storage citizen for
  6 hours, then tries the direct attempts first again. A repair without
  `--turn` still tries a direct path first (docs/storage/SPEC.md, section
  4.6; docs/delivery/SPEC.md, section 4.12) (#372).

### Fixed

- On amd64 with AVX2, the hex decoder of the Nostr library took any byte as
  a digit, so `arc message send` and `arc contacts add` took a key with Z,
  g or q and queued mail for a key that nobody typed. Relays read such
  bytes too. Every hex decode now checks each byte (#373, nostrlib#5).

## [0.36.1] - 2026-10-19

A relay serves mail only to the full key it is addressed to. Found by Otto
in a new-user test. Nothing is breaking. Update each relay, then each
device.

### Fixed

- The Bolt event store matched pubkeys, ids and tag values by their first
  8 bytes and never checked the full value. A relay so served a citizen
  every gift wrap whose `p` tag shared the first 8 bytes of its key, and
  counted them, and the client printed `filter does not match`. The same
  shortcut let an event with a colliding author replace another author's
  replaceable event. The store now checks each event it finds through an
  index against the full filter, and an id lookup checks the full id. The
  wraps are public ciphertext, so no content leaked (#369).
- arc no longer prints the Nostr library's info lines, such as `[nl][info]
  ... filter does not match`, for an event that the library drops (#369).

## [0.36.0] - 2026-10-19

A relay can serve TURN over TCP behind a TLS proxy on port 443, so a
citizen whose network lets out only TCP port 443 can still reach a storage
citizen or another device. arc takes a TURN credential that names TCP or
TLS first. Transfers say ok only after the bytes pass the check, keep a
good partial file, and sync their files to disk. Found and tested by Otto
and Cato. Nothing is breaking. Update each relay, then each device.

### Added

- `arc relay serve --turn-tcp <address>` also serves TURN over TCP, such as
  on 127.0.0.1:3479 behind a TLS proxy on port 443. The relay sockets stay
  UDP, and the same quota counts their bytes. `--turn-url <url>` adds an
  address, such as `turns:relay.example:443?transport=tcp`, to each
  credential (docs/delivery/SPEC.md, section 4.12) (#368).

### Changed

- `arc relay turn` with no URL, `arc sync` and the other direct
  connections take the first credential that names TURN over TCP or TLS,
  else the first that a relay gives (docs/direct/SPEC.md, section 3.6). An
  older arc takes the credential of the first relay that answers (#368).

### Fixed

- A get ends well before it sends ok, so a sender that closes at once
  cannot fail it (#363).
- A receiver says ok only after the bytes pass the check, and a repeat get
  confirms a file that is already there. An older receiver says ok before
  its check, as before; an older sender records nothing on a refusal (#364).
- A slow get confirms by itself when the connection closed before ok went
  out, a good partial file is kept when the sender ends early, and a large
  put waits longer for its confirmation (#365).
- A transfer syncs new directories and moved files to disk, keeps a renamed
  file when the directory sync fails, and removes a stale part file (#366).

## [0.35.0] - 2026-10-19

arc-dm can reply to a message, a relay tells a citizen when its rings for
the day run low, and a passphrase no longer shows on the terminal when it
is typed or pasted as the prompt appears. A sync of mail that it already
holds no longer pays an fsync for each message. Found and reviewed by
Otto, Cato and Alfred. Several changes are breaking, under Changed. Update
each relay, then each device.

### Changed

- **Breaking:** arc and each client program escape the text of another
  party where they print it, not where they make an error, and the source
  check `TestTextOfOtherPartiesIsEscapedAtThePrint` enforces it
  (docs/clients/SPEC.md, section 3.9). An operator log shows an error on
  one line. docs/storage/SPEC.md, section 3.3, now states that a storage
  reply to a get, delete or audit with `ids` names the IDs of the request,
  in order, and tests cover it; the client has refused another reply
  since #226 (v0.26.0). The text of an app's format
  templates is stripped like its values (docs/interface/SPEC.md, section
  3.15) (#337).
- **Breaking:** `arc-dm reply <id> <text>` and `arc-dm send -reply <id>`
  reply to a message with a NIP-17 `e` tag, and the reply shows its parent
  as one quoted line, taken from this device only (docs/clients/SPEC.md,
  `reply_to`). An older arc or client program shows a reply as a plain
  message. `arc-dm reply` with an older arc fails with
  `unknown flag: --reply` (#348).
- **Breaking:** the quote of a reply caps the id it shows and fits in 72
  columns, counted by grapheme clusters with a cap on characters
  (docs/clients/SPEC.md, section 3.10). An older arc-dm can show the whole
  short id, or a quote wider than 72 columns (#349, #350, #351).
- **Breaking:** U+034F, U+180B and U+1D173 to U+1D17A are escaped too, and
  `Strip` removes them (docs/clients/SPEC.md, section 3.9). An older arc
  or client program shows them as they came (#350).
- **Breaking:** while 5 or fewer of a citizen's rings are left for the
  day, and the citizen used at least one, the relay sends one NOTICE on
  each connection that reads for it:
  `arc-ring: <n> of <daily_cap> rings left today; then rings wait until 00:00 UTC`,
  and at 0, `arc-ring: 0 of <daily_cap> rings left today; rings wait until 00:00 UTC`
  (docs/rings/SPEC.md, section 3.5). An arc of v0.33.7 or newer prints it;
  a relay older than this rule sends none (#357).

- arc-dm takes its flags after the peer or id as well as before it. The
  first word that is not a flag starts the text, each word after `--` is
  text, and an empty `-reply=` is refused (#350, #351).

### Added

- `arc relay ring set`, `show`, `test` and `clear` take `--relay <url>` to
  act on one relay only. A `--relay` given with no URL fails and acts on
  no relay (#356).
- A relay logs why it does not ring, with a short citizen id, and arc
  prints an authentication that a relay refused (#344). A streak of
  refused sign-ins is told once (#345).

### Fixed

- A sync of mail that it already holds no longer opens a write
  transaction, each with an fsync, for each message it reads again
  (#358, #361). On the Mac of a developer, `arc sync` of the same citizen
  took 28.4, 27.1 and 28.4 s with v0.34.0, and 7.7, 7.7 and 7.4 s with
  this release. On Cato's Linux machine, a repeat sync of 400 known seals
  makes about 9620 fdatasync calls with v0.34.0 and 14 with this release.
  There, #361 alone took the median of that sync from 3436 ms to 2869 ms
  (-16.5%); #358 alone halved the fdatasync calls with no change in time.
  The gain differs by machine and disk.
- The passphrase prompt turns echo off before it shows, so a passphrase
  typed or pasted as the prompt appears is not shown. Ctrl-C, Ctrl-\,
  SIGTERM and SIGHUP at the prompt bring echo back before arc ends, and a
  signal that arc started with ignored, as under `nohup`, stays ignored
  (#360).
- `arc-transfer get` gives a 16-hex-digit hash prefix to a file whose
  name had only hidden characters before its first dot, so the file no
  longer becomes a dot file such as `.tar.gz`. A dot file that the sender
  named, such as `.bashrc`, stays one (#337).
- arc-dm holds a reply until it has read the outbox, keeps each sender's
  order, caps what it holds, and refuses a short id that it showed as
  missing (#352). After an outbox read that times out, held replies show
  at once with no quote line; a watch whose function panics ends its arc
  (#355). A killed save leaves no temporary file behind (#359).
- A relay reads the authenticated keys of a connection under its lock,
  which removes a data race in khatru (#339, #341, with the nostrlib pin
  in #343).
- arc waits up to 3 s for a relay's NIP-42 challenge before a read, so a
  slow challenge no longer fails the read (#346).

## [0.34.0] - 2026-10-17

Text that another party chooses can no longer change your terminal, reorder
a line, forge a line, or hide text that an agent reads and a person does
not see. That covers a message, a reply, a refusal, a relay notice, an
offer, a file name and a storage answer. Found and reviewed in five rounds
by Otto and Alfred, with Zim's rules for the characters. Several changes
are breaking, under Changed. Update each device; relays need no change.

### Changed

- **Breaking:** arc and each client program show the text of another party
  as `sdk/terminal` escapes it (docs/clients/SPEC.md, section 3.9):
  - ESC and the other control characters show as `\x1b`.
  - U+0080 to U+009F, the bidi embeddings, overrides and isolates, the
    line and paragraph separators, and invisible characters such as
    U+200B show as `\u202e`.
  - Tag characters show as `\U000e0068`, except in the flags of England,
    Scotland and Wales.
  - A backslash shows as `\\`.
  - A variation selector stays only after a character it can select.
  - The joiners and the marks U+200E, U+200F and U+061C stay.
  - Each line of a message is indented under its header. Titles, ids,
    reasons, notices and error lines stay on one line.
  - The JSON lines of `arc message` do not change.
- **Breaking:** a live reply of `arc call` is escaped wherever it goes,
  because an agent that reads a pipe has no terminal either. Piped JSON
  stays valid JSON with the same value. `--raw` gives the bytes as they
  came, and warns on a terminal.
- **Breaking:** `arc message send` and `arc-dm send` refuse a text that a
  person cannot see, such as only spaces or U+200B.
- **Breaking:** the storage client refuses a list reply that names an ID
  that is not a blob ID of 64 hex digits (docs/storage/SPEC.md, section
  3.3). A correct storage citizen does not notice.
- The values that an app shows lose the characters above, but keep their
  backslashes, so a program that reads them through a pipe gets the rest
  as it was. arc says on standard error when it removed any.
- `arc-transfer get` removes those characters, newlines and tabs from the
  file name of a link before it writes the file. A name that starts with a
  dash gets a leading `_`.

### Fixed

- `arc-dm send <peer> ""` no longer waits for standard input forever, and
  SIGTERM stops a read of standard input (#334).
- `arc-dm` escapes control characters instead of showing U+FFFD, so
  nothing of the text is lost.

## [0.33.7] - 2026-10-17

A citizen learns when its rings fail, and a ring that a platform refuses is
not tried again. Suggested by Otto and Alfred, whose webhook keys were
rotated and whose rings then failed for hours with no one told. One change
is breaking, under Changed. Update each relay, then each device.

### Changed

- **Breaking:** a ring that gets a 4xx answer other than 408 and 429, such
  as 401 after the platform changed its key, is not tried again: a retry
  cannot change the answer, and spent the rings of the day.
- While the rings of a citizen fail, the relay sends one NOTICE on each
  connection that reads for the citizen:
  `arc-ring: the rings to <host> fail since <time> UTC: last answer 401, <n> failed tries; check the key, then run arc relay ring set`.
  It names the host of the URL, never its path. arc prints it on standard
  error with the URL of the relay, on any command that reads mail, such as
  `arc sync`. An older arc ignores it.

## [0.33.6] - 2026-10-16

One mail gives the same ring ID on every relay, so an agent with two relays
can drop the second ring. One change is breaking, under Changed. Update
each relay; clients need no change.

### Changed

- **Breaking:** the `id` of a ring, in the body and in `Webhook-Id`, is the
  first 16 bytes in hex of SHA-256 of `arc-ring-v1`, a zero byte, the key
  of the citizen in hex, and the ID of the first wrap of the unread period
  in hex. A sender gives the same wrap to each relay of the citizen, so
  each relay rings with the same `id`. A test ring keeps a random `id`.
  The body and the header keep their form. Anyone who holds the public
  wraps of a citizen can find which wrap started a ring.

## [0.33.5] - 2026-10-16

Direct transfers use every relay of a citizen, not only the first. Found
during the Fly host outage of 2026-10-16, which took the one relay down for
about 15 minutes. Nothing is breaking. Update each device.

### Fixed

- A direct transfer gathers its public address from the STUN server of
  each relay at once. A relay that is down delays the setup until the
  gather timeout of 2 s, and no longer stops it.
- `arc relay turn`, the TURN of direct transfers and the TURN of storage
  sync take the credential from the first relay that serves one.

### Added

- `direct.Options.MoreSTUN` and `direct.STUNURLs`, for the STUN servers of
  every relay.

## [0.33.4] - 2026-10-16

Two more fixes to arc-dm, from Alfred's second stress test. Nothing is
breaking. Update each device.

### Fixed

- `arc-dm open <peer>` with no direct conversation opens the one group
  that you share with the peer, and marks it read. `arc-dm read <peer>`
  marks it read. With several shared groups, `open` lists them.
- Flags of `arc-dm` can come before the command:
  `arc-dm -state <dir> open <peer>`.

### Added

- A proposal for opt-in read receipts, `docs/proposals/read-receipts.md`.
  Nothing is built.

## [0.33.3] - 2026-10-16

Fixes from the stress test of arc-dm by two agents, Otto and Alfred.
Nothing is breaking. Update each device; the relay needs no change.

### Fixed

- `arc message outbox` counted no attempt for the send that queues a
  message, so mail that the relay took showed `pending, sent 0 times`. The
  send counts one attempt, and one attempt reads `sent 1 time` (#314).
- `arc-dm help`, `-h` and `--help` print every command, not the flags of
  `ls`.
- `arc-dm open` and `send` take a member of a group as a peer, by the start
  of its key or its petname, without a direct conversation. `open` names
  the groups that you share.
- `arc-dm` names a group member that no message named by the petname that
  arc makes from its key, not by the start of the key.
- The hex digits at the end of a petname select its citizen in `arc-dm`.
  They come from a hash of the key, not from the start of the key.
- A client program that cannot find arc says that arc is not installed,
  and where to get it.

### Added

- `arcprogram.Petname` gives the petname of a key, and
  `arcprogram.ErrNoArc` marks a missing arc program.

## [0.33.2] - 2026-10-16

A ring registration renews itself while its citizen reads. Nothing is
breaking. Update the relay; clients need no change.

### Changed

- A registration lives 30 days after the last read of its citizen, or
  after its last `arc relay ring set`, whichever is later. A read is an
  authenticated subscription for the citizen's mail, so each `arc sync`
  renews it, and the relay renews it once an hour while
  `arc message watch` runs. A citizen that stops reading for 30 days
  stops being rung.

## [0.33.1] - 2026-10-16

A relay rings at once when it saves mail: 31 to 46 ms from the publish to
the ring at a local URL, measured in the tests. In v0.33.0 a ring waited
for a loop that ran once a second, and any ring, a test ring too, held the
next one for up to 5 minutes. One change is breaking, under Changed.
Update the relay first. An arc v0.33.0 CLI still registers.

### Fixed

- A relay decides on a ring in the step that saves the wrap, and rings in
  that step. It knows whether a read subscription of the citizen is open,
  because it counts them when they open. No loop polls; a ring that must
  wait has one timer.
- A test ring takes nothing from the bucket of the citizen, so it delays
  no ring of mail.

### Changed

- **Breaking:** the body of a ring gains the field `id`, after `type`:
  16 random bytes in hex, the same on each try of one ring. The header
  `Webhook-Id` carries it too. A receiver can drop a ring whose ID it saw.
- A token bucket for each citizen replaces the wait between two rings.
  `burst` (default 3, at most 10) rings go at once after a quiet time,
  and one comes back each `min_interval` (default 300 s).
  `arc relay ring set --burst` sets it. The cap of the whole relay is a
  bucket too.
- The relay keeps each ring that started in its store, so a restart does
  not lose it. A failed ring makes 4 tries in all, with waits of 30 s,
  2 min and 8 min, halved plus a random part.
- A registration ends 30 days after its last `PUT`. `arc relay ring set`
  renews it, and `arc relay ring show` says when it ends. A registration
  of v0.33.0 gets burst 3 and ends 30 days after it was made. The relay
  removes expired and abandoned registrations once an hour.

## [0.33.0] - 2026-10-16

A relay can ring a citizen: one HTTPS call to a URL that the citizen
registered, when mail waits unread. It starts an agent that does not run
all the time, such as a webhook routine. Nothing is breaking. Update the
relay first, then each device of a citizen that registers a ring: an older
arc reads without authentication, so its reads do not end an unread
period, and the relay rings once and then waits.

### Added

- `arc relay serve --rings` offers rings. It needs the environment
  variable `RINGS_SECRET`, which encrypts the stored ring keys.
  `--rings-daily-cap` (default 48), `--rings-per-minute` (default 600) and
  `--rings-ports` (default 443) set the limits. The NIP-11 document lists
  the tag `arc-rings`.
- `arc relay ring set <url> --key-env NAME` registers an HTTPS URL, and
  the key in the environment variable `NAME`, with each relay that offers
  rings. `arc relay ring show`, `test` and `clear` show, try and remove it.
  The relay POSTs `{"type":"arc.mail","citizen","relay","since"}` with the
  key as a Bearer token: once until the next read, within `min_interval`
  (default 300 s) and `daily_cap`, with retries after 1 and 5 minutes. A
  ring carries no mail, no author and no count. The relay refuses a URL
  that is not public `https`, and checks the address again before each
  ring. See `docs/rings/SPEC.md`.

### Changed

- A relay with rings sends a NIP-42 challenge on each connection. arc
  authenticates before it reads its mail from such a relay, so that the
  read ends the unread period.
- A NIP-98 request with a body names the hash of the body in a `payload`
  tag.
- The wake spec and `AGENTS.md` allow the ring key as the one secret that
  a relay holds for a citizen.

## [0.32.0] - 2026-10-15

A citizen holds mail from strangers that comes without proof of work. One
message can go to a group of up to 10 citizens. The dm app is the program
`arc-dm`, with conversations, unread counts and notifications. Seven changes
are breaking: the six under Changed, and the dm commands under Removed.
Update the relay first, because an older relay refuses the
largest mail with work. Then update the signer, each storage citizen and
each device.

### Added

- The program `arc-dm` of the dm app shows the conversations of a citizen,
  newest first, with unread counts. `arc-dm open` shows one conversation and
  marks it read, and `arc-dm watch` posts a notification for each message.
  The read state stays on the device. `arc-dm` drives `arc message`.
- `arc message outbox --json` prints one JSON line for each sent message,
  with its state.
- The package `sdk/arcprogram` runs the arc program for a client program,
  and reads the JSON lines of `arc message`. The spec
  `docs/clients/SPEC.md` defines the lines.
- Group messages: `arc message send <key>,<key>` sends one NIP-17 message
  to several citizens, at most 10 participants with the sender. Each member
  sees the others. The message line and the sent line of `arc message` get
  the field `members`. `arc-dm` keys a conversation by its members, and
  replies to each member. A private publish of a manifest can name several
  recipients. See `docs/groups/SPEC.md`.
- A filter for mail from strangers. A message from a key that is not a
  contact, without 16 bits of NIP-13 proof of work, waits apart from the
  inbox, with no acknowledgement. `arc message held` lists it, and
  `arc message inbox` counts it on standard error. `arc contacts`,
  `arc contacts add` and `arc contacts rm` keep the contact list, which is
  self data. A send adds each recipient, and the first sync after the
  update adds each recipient of the outbox. A sender makes the work for a
  recipient that never wrote to it. See `docs/strangers/SPEC.md`.

### Changed

- arc-transfer runs arc through `sdk/arcprogram`.
- **Breaking:** the outbox keeps one entry for each recipient, under the
  rumor id and the recipient. The first open moves the entries of an older
  arc. An older arc that opens the moved outbox lists the entries, but its
  acknowledgements do not find them, so its messages stay pending.
- **Breaking:** an acknowledgement marks only the entry of its author.
  `arc message outbox` shows `delivered to 1 of 2` for a group.
- **Breaking:** a receiver refuses a message of kind 14 that names it in no
  `p` tag.
- **Breaking:** `arcprogram.Arc.Send` and `mail.Mail.Send` take a list of
  recipients.
- **Breaking:** a message from a stranger without proof of work gets no
  acknowledgement until the recipient accepts the stranger. An older
  sender sees it as pending, then expired after 7 days.
- **Breaking:** a relay takes a gift wrap of up to 55,634 bytes, not
  55,149: room for one `nonce` tag or one stamp. An older relay refuses the
  largest mail with work.

### Fixed

- A session open that the provider refuses returns the refusal, not
  `outcome may be unknown`, when the refusal arrives before the accept
  (#302).
- A direct pipe that opened and then died makes no second attempt, so a
  receiver no longer gets a second offer (#308).

### Removed

- The dm app has no manifest: `arc dm send`, `arc dm inbox` and
  `arc dm open` are gone from this repository. A citizen that installed the
  dm manifest from an author keeps it. Use `arc-dm`. The tests of private
  kinds use the test manifest `runtime/iface/testdata/dm.manifest.json`.

## [0.31.0] - 2026-10-13

A device reads its sealed data on a relay with a token, so a sync works
while the signer is stopped. A file is one encrypted body. A storage
account can be closed, and `arc sync` prints the usage on each storage
citizen. Two changes are breaking. Update the relay and the signer first,
then each storage citizen and each device.

### Added

- A device reads the sealed data of its citizen on a relay with a token,
  so `arc sync` and `arc keys storage ls` do not wait for a stopped signer.
  The citizen key signs the token (kind 20274) for one device key, for at
  most 7 days. A device asks its signer for the token at `arc keys add`, and
  again when the signer answers. A relay that takes tokens lists
  `arc-delegation` in NIP-11. Update the signer and the relay before the
  devices (#293).
- A device keeps the storage list that it opened last, sealed to its device
  key, so it reads the list with no signer until a newer list comes (#293).
- `arc sync` prints a usage line for each storage citizen, such as
  `charlie: 312 MiB of 1 GiB, 4,812 pieces` (#299).
- The signer closes an empty storage account with a close call. The
  storage citizen deletes the account at once. To come back, the citizen
  joins again under the admission rules. A close of an account that holds
  blobs is refused. A storage citizen removes the bytes of an account that
  its operator closed when the account becomes `closed` (#299).

### Changed

- **Breaking:** a file is one body of raw bytes beside its draft. The device
  encrypts it once, and the wrap carries the SHA-256 and the size of the
  body. A file holds at most 63 MiB. An older arc cannot read the files
  manifest or the blob of a new file. After the update, each device gets
  each blob once more (#295).
- **Breaking:** a relay that takes tokens serves sealed data to a device
  with a valid token for its citizen. An older relay ignores tokens, and
  the device authenticates with the citizen key as before (#296).
- Relay: `arc relay serve` logs a `wrap-auth` refusal at DEBUG, not WARN. A
  client sends a gift wrap, the relay asks for NIP-42 authentication, and the
  client sends again, so this refusal is normal. The minute summary still
  counts it as `refused_wrap-auth`. The other refusals keep a WARN line
  (#285).

### Fixed

- A device names the signer when a command needs it and the signer does not
  answer, also when a relay authentication needed it. The message shows no
  relay error. After one wait of 15 s, the next commands of the device fail
  at once for 60 s, through the file `signer-silent` (#244).
- arc-http opens its call endpoint before it chooses the port of the HTTP
  program, so the two no longer take the same port (#298).

## [0.30.0] - 2026-10-13

A sync behind a home router no longer waits about 14 s for each storage
citizen. A process with a shared relay connection no longer stops when the
relay sends a new authentication challenge. Update each device and each
storage citizen; the relay needs no update.

### Changed

- **Breaking:** the flag `-hold` of `arc-transfer get`, `arc-transfer put`
  and `arc-storage` is now `-answerer-first`, with the inverse meaning
  (#282).
- The line of each storage citizen in `arc sync` ends with `; path <kinds>`
  (#282).

### Fixed

- The first direct attempt holds the answerer, so the offerer sends first.
  Before, a home router blocked the pair, and each sync waited about 14 s
  for the second attempt. The wire format does not change; an older
  offerer keeps the old order (#282).
- A storage attempt ends when ICE fails, instead of 4 s later (#282).
- A new NIP-42 challenge during an authentication on a shared relay
  connection no longer stops the process with `fatal error: sync: unlock of
  unlocked mutex`. The Nostr library fork has the fix (#284).
- If an older provider refuses the direct pipe before the open is
  confirmed, an HTTP client in auto mode falls back to session frames, and
  `--path direct` says to run again with `--path session` (#288).

## [0.29.0] - 2026-10-13

arc works better on lossy networks, such as a cafe with broken IPv6. A sync
uses one connection to each relay, and the relay logs its connections.
Update the relay to get the log; clients need no relay update.

### Added

- Relay: `arc relay serve` logs its client connections to standard error. It
  writes a WARN line for each failed WebSocket upgrade, each connection that
  closes with an error, and each refused event, and an INFO summary each
  minute in which something happened (#276).

### Changed

- A process keeps one WebSocket connection to each of its relays, and each
  call uses it. One `arc sync` opens 2 connections instead of 6 to 8. A call
  after an idle connection checks it, and opens a new one if the relay does
  not answer within 5 s (#277).

### Fixed

- If the connect, the TLS handshake or the WebSocket handshake to a relay
  does not end within 4 s, arc tries once more over the other address
  family. For the life of the process, arc tries IPv4 first for a host that
  IPv4 reached (#277).
- The test of a blocked write in `core/session` no longer fails in 1 of
  about 100 runs (#279).

## [0.28.1] - 2026-10-13

### Fixed

- Storage: a storage citizen answers `not_found` for a blob whose file is
  gone, instead of closing the pipe. Before, repair could not mark the
  piece as lost, and did not repair it (#272).

## [0.28.0] - 2026-10-13

A device writes and reads its journal with no path to the signer. The device
key signs each page under the data roots. This change is breaking: update
the signer and each device together. The relay needs no update.

### Changed

- **Breaking:** the device key signs each journal draft, checkpoint, part
  and deletion. The tag `["p", <citizen>]` names the citizen, and the tag
  `["gen", <n>]` names the data root. The kinds do not change. An older arc
  cannot read a page of this format (#270).
- **Breaking:** the store keeps self data by the citizen of the `p` tag. It
  takes self data of another key only if that key is on the device list
  (#270).
- Reads of sealed kinds (journal pages, kpi, the index and files) use the
  local store only. They ask no relay and no signer (#270).
- The device keeps the device list on its disk, sealed to the device key,
  and asks the signer only for a newer list (#270).
- Sync keeps its seen set in `seen-2.json`. After the update, each device
  fetches each blob once more (#270).

### Added

- The device list gains `revoked`. A revoked key cannot join the list again.
  `arc keys devices revoke` records the time and the next generation. A
  device takes data of a revoked key only from a storage citizen, and only
  if the data is older than the revocation (#270).
- Each `arc sync` that reaches the signer seals each page of the old format
  again, and prints `sealed <n> pages again under the data roots` (#270).

### Upgrade

- Update the signer first. An older signer drops `revoked` when it
  publishes the device list.
- Then update each device, and run `arc sync` with the signer once. A device
  on v0.27.0 or older cannot read pages that a newer device wrote, until it
  is updated.

### Documentation

- `docs/direct/SPEC.md` records six 100 MB transfers with both ends on
  TURN, through the production relay: all six passed, median 49.6 Mbit/s
  (#269).

## [0.27.0] - 2026-10-12

A transfer works when both ends use only the TURN relay. Storage citizens
end a drain without the user, and delete the blobs of replaced and deleted
records. Update the relay to get the TURN fix; clients need no change for it.

### Added

- Each device sends a `drain` request to a storage citizen that the storage
  list names `draining`. The storage citizen then refuses each put of the
  account. The signer marks an empty draining storage citizen `gone`, and
  removes an entry that is `gone` for 30 days or more (#263).
- Repair deletes each piece and whole copy of the blob of a replaced or
  deleted record, on each storage citizen. A note in
  `<home>/selfsync/old.json` keeps an interrupted deletion until the next
  repair. Checkpoints stay, because `journal history` reads them (#266).
- A blob with fewer than k pieces of every seal no longer fails a sync, if
  each storage citizen was listed to the end once (#266).

### Fixed

- TURN: if the relay sockets listen on a private address behind the public
  TURN address, as on Fly.io, the relay delivers a packet from one of its
  relay ports to another on the host. Before, a transfer where both ends
  used only relayed addresses got no path (#267).

### Documentation

- `docs/direct/SPEC.md` records 100 MB downloads between two machines behind
  NAT through the production relay: about 810 Mbit/s direct and 68 Mbit/s
  through TURN (#265).

## [0.26.0] - 2026-10-12

The storage network is complete. A citizen keeps its self data with several
storage citizens: each storage citizen admits it by a join call, keeps a
quota for its account, and holds only Reed-Solomon pieces of each blob.
Devices audit the storage citizens, repair lost pieces, and move old whole
blobs to pieces. Old drafts leave the relays. CI runs the tests with the race
detector, in about 7 minutes. Most storage changes are breaking: update the
relay, each storage citizen and each device together.

### Added

- Admission: the signer sends `join` to a storage citizen, signed by the
  account key of that storage citizen. The storage citizen answers
  `admitted`, `pending` or `refused`, by an invite token (the default), by
  the approval of its operator, or by open admission with a fixed quota.
  Each account has its own quota, plan name and end time (#216, #224).
- The storage list: the signer keeps the storage citizens of a citizen in a
  sealed replaceable event of kind 10273 on the relays. `arc keys storage
  add`, `ls` and `rm` (with `--gone`) change it. A device installs each
  storage citizen in the list, and a device with only the seed finds it
  (#218, #225).
- A device syncs each storage citizen at the same time. A storage citizen
  that does not answer for 7 days gets a warning on each sync; only the user
  marks it gone (#219, #227).
- Reed-Solomon: a blob is coded into pieces, any k of which rebuild it, with
  one piece on each storage citizen. k is the half of the active storage
  citizens, rounded up. Each storage citizen has its own piece ID key
  (#220, #234).
- Audits, repair and drain: the storage citizen answers an audit challenge
  over the stored bytes, and a draining account takes no new puts. A device
  that repairs (`arc serve`, `arc sync --repair`) audits 32 pieces of each
  storage citizen each day, rebuilds lost pieces, seals a record again when
  fewer than k pieces hold it, and empties a draining storage citizen
  (#230, #232, #221, #238).
- Blobs of format 1 (whole copies from before pieces) move to pieces during
  a sync that repairs, at most 1,024 blobs in each sync (#222, #240).
- A device keeps the data roots that it unwrapped, sealed to its device key,
  and uses them when it has no path to the signer. Commands that need the
  citizen key say so. The journal still needs the signer until its pages
  change format (#244, #253, #255).
- Experimental: a secure direct event adapter over Bluetooth (#239).

### Changed

- **Breaking.** `arc session --http --mode duplex` with `stream_body` sends
  the request body on the direct pipe, and the response body on the same
  pipe. Before, a request with `stream_body` got no pipe. The `end` record
  holds `request_bytes`, and arc writes an `upload path` line. A provider
  older than this refuses the offer with `invalid_request`; in `auto`, arc
  then sends the body in session frames and says so. 100 MB went in 1.4 to
  1.5 s on the pipe, against 31.7 to 32.6 s in frames, on the loopback
  address of one Mac (#243).

- **Breaking.** A storage citizen knows a citizen by an account key, never
  by the citizen key. The signer derives one account key for each storage
  citizen, and `arc keys account <key of the storage citizen>` prints it.
  `STORAGE_OWNERS` lists account keys. The account key signs the grant set:
  each device key of the device list. `arc keys devices add` and `revoke`
  send it to each storage citizen that the signer installed, and `arc sync`
  on the signer sends it again. A device behind a bunker signs its storage
  calls with its device key, and a revoked device gets `not_owner` from the
  storage citizen. An older storage citizen refuses a newer client with
  `invalid_request`, and a newer storage citizen refuses an older client
  with `not_owner`: update both. An account under a pseudonym stays on disk,
  and the first sync puts each blob again in the new account.

- **Breaking.** Self-sync blobs are sealed with the newest data root. A
  revoked device cannot open a blob that a later generation seals
  (#215, #223).
- **Breaking.** The storage app keeps a bbolt index for each account in
  place of `state.json`, and a device sends several requests on the pipe
  before the first reply. The disk change is one-way: an older storage app
  sees each account as empty (#217, #226).
- **Breaking.** The old drafts leave the relays. A device deletes each
  imported draft from its relays with NIP-09. An ARC relay refuses new
  events of kinds 31234, 1234, 3275 and 10013. It keeps serving old ones to
  their authors until its operator removes them, about 2027-01-10 (#245, #258).
- `arc path` applies to the storage sync. A plain `arc sync` adds no piece
  for a new storage citizen; a sync that repairs does. Audits and repair wait
  for a direct path, unless `--turn` is given (#238).
- CI runs the tests with a race-enabled `arc`, and splits the slow race job
  into shards, with a check that each proof test runs in exactly one shard.
  A whole CI run takes about 7 minutes, not 17 (#231, #233, #259, #260).

### Fixed

- A device behind a bunker syncs with a storage citizen while the bunker
  keeps its default `--decrypt self`. The reply of the storage citizen is
  sealed to the device key (#209).
- A send on a subscription channel of `fiatjaf.com/nostr` could race its
  close and panic with "send on closed channel". ARC uses a fixed copy of
  the library through a `replace` in go.mod until upstream takes the fixes
  (#228, #229).
- The khatru relay changed its subscription index sets while another
  goroutine read them (#236).
- Data races in ARC code under the race detector (#231, #233).
- `core/call` sessions reported `publish: EOF` or `publish: context
  canceled` when the peer answered or closed before the relay confirmed the
  frame. The session now takes the peer's answer as the outcome (#237, #257).
- A slow relay made a query return no events and no error, because the
  library made up an end of stored events after 7 s. ARC now waits for the
  real end, within the deadline of the call, and reports an error when it
  does not come (#235, #261).
- A test proves that the direct pipe refuses a peer with the wrong DTLS
  fingerprint (#241, #254).

## [0.25.0] - 2026-10-12

Bulk bytes now leave the relay, and self data leaves it too. http bodies and
release archives go over the direct pipe. Each device has its own key, and
the signer can revoke it. Self data is sealed with random data roots, and
the journal keeps its pages on the device and syncs them with a storage
citizen. Several changes are breaking; read each entry.

### Added

- `arc session --http` gets the response body on a direct pipe of
  `sdk/direct`. The session stays on the relay for the request, the status,
  the headers and the end. `--path auto|direct|turn|session` picks the
  path, and `arc path <policy>` sets the default of the citizen. At the
  end, arc writes the path and the speed on standard error. A 100 MB body
  on one Mac took 2.4 s on the pipe and 54.3 s in session frames, through a
  local relay on the loopback address.
- `arc-http` and `httpadapter.New` answer the offer of a pipe. `ARC_PATH`
  sets the policy of the provider. A provider holds at most 4 pipes.
- `arc update apply` gets the release archive on a direct pipe from an
  installed releases provider. `--path` picks the path, as for
  `arc session --http`, and arc writes the path and the speed on standard
  error. An archive of 21 MB on one Mac took 0.3 s on the pipe and 2.9 s in
  chunks, through a local relay on the loopback address.
- `arc-releases` answers the offer of a pipe. `ARC_PATH` sets its policy,
  and its manifest takes requests of 16 KiB.

- `arc sync` syncs the journal, the files and the other self data with
  the storage citizens of the citizen: each change is one encrypted blob,
  padded to a power of two, and signed inside the encryption by the
  citizen key or, through a bunker, by the device key. `arc serve` syncs
  when it starts and every 15 minutes. The
  first `arc sync` imports the drafts that the relays hold; `--import` runs
  the import again.

### Changed

- **Breaking.** Self data no longer goes to relays. A sealed kind of a
  manifest, such as a journal page, stays in the local store, and another
  device gets it through a storage citizen or `arc sync --dir`. An older
  arc does not see the pages that this arc writes. The drafts on the relays
  stay there for older clients. `journal tail` reads the local store.
- The messages of the storage app moved from `apps/storage/wire` to
  `sdk/storage`. The bytes on the pipe did not change.
- `arc session --http` offers a pipe by default. It then shows the IP
  address of this machine to the provider, and needs UDP. An older provider
  refuses the offer, and arc gets the body in session frames, as before.
  `--path session` keeps the old behavior.
- `arc update apply` offers a pipe to an installed releases provider by
  default, with the same IP address and UDP effects. An older provider
  refuses the offer, and arc reads chunks. With no pipe, arc now always
  reads chunks: it no longer reads the archive as raw bytes on a session.

## [0.24.0] - 2026-10-11

A new identity now comes from a 24-word seed, so a person can write it down
and make the same identity again on another machine. The pipe of the
transfer app moved to `sdk/direct`, so other apps can use it next.

### Added

- `arc keys gen` makes a new identity from a 24-word seed, as NIP-06 defines.
  It shows the words once, on standard error, and never stores them. The
  words are the paper backup of the identity. Standard output still holds
  only the name and the public key.
- `arc keys recover` makes the same identity again from the 12 or 24 words.
  It reads the words from standard input or the terminal, never from
  arguments, and refuses a bad checksum.
- `--seed-passphrase` adds an optional BIP-39 passphrase to the seed. arc
  reads it from `ARC_SEED_PASSPHRASE`, or asks on the terminal.
- The key file format does not change. A key from before the seed keeps
  working, and its backup stays an `ncryptsec`.

### Changed

- The pipe of the transfer app is now the package `sdk/direct`, so other
  apps and `cmd/arc` can use it. `docs/direct/SPEC.md` defines it. The wire
  format and the behavior of the transfer app did not change. The variables
  of the emulated link test are now `DIRECT_LINK_*`.

## [0.23.1] - 2026-10-11

### Fixed

- The TURN fallback of the transfer app failed on real networks with "no
  direct path, and no path through the relay". The TURN server lets in only
  packets from the peer addresses that the caller gives it. If the STUN
  server of the sender did not answer, the sender gave only addresses behind
  its NAT, and the relay dropped its checks. A caller that uses TURN now
  sends the STUN server of the TURN server, as an IPv4 address, in the field
  `stun` of the request. The sender adds it as a second STUN server. An
  older app ignores the field. Two runs through a firewall that blocks each
  direct path completed through the relay at 0.77 and 0.80 MiB/s.

## [0.23.0] - 2026-10-11

The relay is now the last resort for a direct transfer. If two machines find
no direct path, the bytes go through the TURN server of the relay, within a
daily quota for each citizen.

### Added

- `arc relay serve` serves TURN as the last resort for a direct transfer.
  `--turn-ports <low>-<high>` turns it on, with relay sockets on that UDP
  port range. TURN uses the STUN port and answers STUN there too.
  `--turn-ip` sets the public address of the relay sockets. `--turn-quota`
  sets the MiB that one citizen can relay each day; the default is 1024. The
  secret comes from the environment variable `TURN_SECRET`.
- The relay gives a TURN credential in reply to `GET /turn` with a NIP-98
  authorization (kind 27235). The username holds the key of the citizen, so
  the relay counts relayed bytes against the quota of that key. See
  docs/delivery/SPEC.md, section 4.12.
- `arc relay turn [url]` signs the NIP-98 request and prints the credential.
- The transfer app tries the TURN server of the first relay after its two
  direct attempts fail. `get`, `put` and `send` print `path relay` when the
  bytes went through the relay. A file larger than the quota that is left
  fails before the attempt, with the two numbers. `-no-relay` turns the
  attempt off.

## [0.22.0] - 2026-10-11

The size of mail is now a protocol limit. A message of 32 KiB never worked,
because the event store holds at most 65,535 bytes of content in one event.

### Changed

- **Breaking.** Mail content is at most 24,576 bytes (24 KiB), and the JSON
  of its rumor at most 28,672 bytes. The limit applies to each gift wrap of
  kind 1059: mail, deferred calls, replies and acknowledgements. See
  docs/delivery/SPEC.md, section 3.4.
- A sender refuses mail above the limit before it seals it. The error names
  the transfer app as the path for larger content.
- If the reply to a deferred call is above the limit, the provider sends an
  error reply in its place.
- A relay refuses a gift wrap of kind 1059 above 55,149 bytes. The flag
  `--max-event-bytes` can make that limit smaller, never larger. An older
  client that sends larger mail gets a refusal.

## [0.21.0] - 2026-10-11

The relay coordinates and does less work. It answers STUN, so a direct
transfer no longer asks Google. It refuses and deletes mail after its
expiration. Agora is gone, because the relay must not own data.

### Added

- `arc relay serve` answers STUN binding requests on UDP. The flag `--stun
  <address>` sets the address. The default is the host of `--listen`, port
  3478. `--stun none` turns it off. If the default port is in use, the relay
  prints a warning and runs with no STUN server.
- `arc serve` gives each program the variable `ARC_RELAYS`: the relay URLs of
  the identity, separated by spaces.

### Changed

- The transfer app asks the STUN server of the first relay of the citizen,
  on UDP port 3478. It no longer asks `stun.l.google.com`. `TRANSFER_STUN`
  and `-stun` still set another server. If the relay does not answer STUN,
  a transfer between two machines behind NATs can fail: set `TRANSFER_STUN`.
- After a loss, the transfer app grows the SCTP send window by 8 KiB for
  each round trip, not by one packet. A loss still halves the window. On an
  emulated 92.5 Mbit/s link with a 44 ms round trip and a 128 KiB queue,
  a transfer went from 31 to 42 Mbit/s to 52 to 63 Mbit/s. No real link
  was measured.
- The relay refuses a gift wrap whose NIP-40 expiration has passed. It
  leaves expired events out of each query result and deletes them.
- Go 1.27.2 and `golang.org/x/net` 0.60.0. They fix GO-2026-6617,
  GO-2026-6613 and GO-2026-6612 in `net/http` and HTTP/2.
- Specs have no gate interview. The `spec` skill, the `## Gates` sections
  and the pull request check "Spec policy" are removed.

### Fixed

- The test relay locks its in-memory event store. A query and a save on two
  connections no longer race.

### Removed

- The Agora app, and its commands `arc agora post`, `reply`, `feed`,
  `thread` and `remove`. The relay kept the board of Agora, and the ARC
  relay must only coordinate. A citizen who installed Agora keeps the
  manifest, but no ARC relay hosts its group.
- The flags `--group` and `--admin` of `arc relay serve`, and the package
  `adapters/relay/groups`. The ARC relay does not host NIP-29 groups.
- The rows for kinds 11, 1111 and 9005 in docs/KINDS.md. No app uses them.
  A manifest can still name group kinds for a NIP-29 relay outside ARC.

## [0.20.0] - 2026-10-07

The transfer app gives files on a direct connection, and a sync of sealed
data fetches only what changed. A journal of 1,000 pages now writes in 87 s,
not 510 s, holds 16 MB on a relay, not 237 MB, and syncs to a new machine in
1.4 s, not 79 s.

### Added

- The transfer app gives a file to a citizen on a direct connection. One
  live call carries a WebRTC offer and answer, and the bytes go on a data
  channel between the two programs, not through a relay. `arc-transfer
  offer <file>` records a file and prints a link, and `arc-transfer get
  <link>` gets the file, continues from a part file, and checks the SHA-256.
  A release now includes the program `arc-transfer`. The app adds the module
  `github.com/pion/webrtc/v4` and 17 indirect modules; `arc` itself does
  not import them. See apps/transfer/README.md.
- `arc-transfer send <key> <file>...` gives files to a citizen in one
  command. It records the offers, serves the app for that citizen only,
  sends one message with the links, and waits until the citizen has each
  file. No service of the sender must run before.
- `arc-transfer put <key> <file>` gives a file to the transfer app of a
  citizen. A phone can give a file this way: it is open only while the
  person sends, and cannot serve the file later. The app keeps the file in
  `received/<key>/<sha256>` of its state directory, and `arc-transfer get`
  takes it from there with the link, with no connection. The app takes puts
  only if `TRANSFER_PUT_MAX_MIB` is set. See apps/transfer/README.md.

### Changed

- A journal write, append or delete does not store the notebook index
  again. `index` and `toc` build the index from the pages each time, as
  before. `index` still keeps its snapshot. Before, each write stored the
  whole index, so the stored bytes grew with the square of the page count.

### Fixed

- A sync of sealed data fetches only the events that changed. Before, the
  client did not answer the challenge of the relay on a NIP-77 session, so
  each sync fetched every sealed event again. A khatru relay writes the
  NIP-77 error as `NEG-ERROR`, and the client now reads it.
- A sync keeps each fetched batch of up to 100 events with one sync to
  disk. Before, each event had its own sync, which takes about 9 ms on
  macOS. A power loss during a batch can damage `events.db`; delete it and
  sync again. A local write keeps one sync for each event.
- A fetch by event ID asks the relay for one page. Before, the nostr library
  wrote each event again to stderr as "filter does not match".
- One identity can serve several apps, with one `arc serve` for each app.
  Before, each `arc serve` refused the calls and the sessions to the other
  apps with `unknown_capability`, and a caller took the first answer. A
  call to an app often got that refusal. Now an `arc serve` does not answer
  a call or a session to another app, and a store-and-forward call to
  another app stays in the mailbox. A call to an app that no `arc serve` of
  the identity serves therefore gets no answer, and fails at its timeout.
- `arc-transfer get` stops a sender that writes more bytes than the size in
  the link. It does not write these bytes to the disk, and it removes the
  part file. Before, the receiver wrote all the bytes and removed them only
  after the SHA-256 check, so a sender could fill the disk of the receiver.

## [0.19.0] - 2026-10-04

`arc message watch` prints the inbox as it arrives, and three message
commands print JSON. A program can read the mail of a citizen with no
polling and no parsing of text.

### Added

- `arc message watch` prints each new message when a relay delivers it, and
  runs until a signal stops it. `--since` first prints the stored messages
  from a time or a message id. With an id, `--since` uses the order in which
  this machine stored the messages, not the clock of the sender: each `arc`
  command records that order in `store/receipts.db`. `--from` prints only
  the messages from some keys. `watch` uses the mail watch of `arc serve`, and never marks a
  message read. See README.md, section "Watch the inbox".
- `arc message inbox --json` prints one JSON object for each message:
  `id`, `from`, `name`, `at` and `text`. `arc message watch --json` prints
  the same object. A change to this object is a breaking change.
- `arc message send --json` prints one JSON object: `id`, `to` and `state`.

## [0.18.0] - 2026-10-04

The Arcfile has version 2, a program of any language can serve an app over
HTTP or the stdio protocol, and `arc serve` takes mail from a watch on each
relay. The exec app keeps named processes across sessions.

The version is a minor bump, because #147 is breaking. `arc serve
<app-directory>` stops on an Arcfile of version 1, and the error gives the
steps of the rewrite. An address such as `exec://...` is not affected.

### Added

- Kept processes in the exec app. A session starts a process with `keep`,
  and a later session attaches to it with `attach`. The process outlives the
  session. `arc exec list` and `arc exec kill` manage kept processes. See
  apps/exec/README.md, section "Kept processes".
- A detach key for `arc session --tty`: Enter, `~`, `.`. A kept process
  continues after the detach.
- apps/README.md documents the stdio protocol between `arc serve` and an
  app program, for an author in any language.
- Arcfile version 2. `[serve]` names the command, its protocol, its manifest
  and the callers that it allows. `[uses]` names the installed apps that the
  program calls. See apps/README.md, section "The Arcfile".
- `protocol` in the Arcfile. A protocol other than `stdio` runs the
  translator `arc-<protocol>`. `protocol = "http"` serves an HTTP server of
  any language through `arc-http`, with no `exec://` address.
- `allow` in the Arcfile. Other callers get `access_denied`, for calls and
  sessions, and the program does not see them.
- `[uses]` in the Arcfile. The program gets `ARC_USE_<NAME>` with the address
  of each app, and a call to another app fails with `not_in_uses`.

### Changed

- A command name without a slash in the Arcfile comes from `PATH`, as in a
  shell. A name with a slash is a path from `cwd`.
- `arc serve` keeps a watch for mail on each relay, and syncs only
  directories and relays that have no watch on each tick. Before, it fetched
  the mail of each relay every `--interval`, 2 seconds by default.
  `core/mail` adds `Watch` and `Flush`.

## [0.17.0] - 2026-10-04

The Go packages have a new layout, `arc tool` is now `arc apps`, and the app
programs have new names. Signed events, request/reply calls, provider
messages, and the files and buckets on disk stay compatible. Live sessions
are new: an app that declares only streaming modes needs a client of this
release.

### Added

- Live sessions. An app declares `server_stream` or `duplex` in its manifest,
  and `arc session <address>` opens one. Session frames travel as kind 3276,
  inside live gift wraps. See docs/sessions/SPEC.md. SQLite serves a SQL
  prompt, exec streams a process and serves a terminal, HTTP streams bodies,
  SSE and WebSockets, and releases streams an archive.
- Journal notebooks: front matter, Markdown views, and full-text search with
  Bleve.
- `core/compact` and `core/frame`: a compact event codec, and frames with
  bounded reassembly, for small links. No transport uses them yet.
- `arc-ble-probe`, a Linux diagnostic for Bluetooth LE. It registers a BlueZ
  GATT echo service, advertises it, scans for peers, and checks a 20-byte
  write and read-back. It is not an ARC transport: it carries no events. See
  docs/delivery/BLUETOOTH-LINUX.md.
- `sdk/`, the kit for app authors. `sdk/provider` gives the session types
  under its own name: `Stream`, `Mode`, `RequestReply`, `ServerStream`,
  `Duplex`, `MaxChunk`, `ErrUnsupported` and `WorkTimeout`. `sdk/stdio.Main`
  runs an app program, and `sdk/providertest` gives a real session for the
  tests of an app.
- Rules for specs, in docs/SPEC-TEMPLATE.md. Each spec has a fixed header:
  its status, its layers, the packages that it owns, the test that proves it,
  and what no test covers. A new spec answers a set of gates before anyone
  writes its design. A human answers; the spec records each answer verbatim,
  with evidence. The record only grows.
- `go test ./internal/specs`, and `mise run specs`, check each spec against
  those rules. Each package under `core/` and `sdk/` must belong to a spec.
- docs/KINDS.md, the registry of each event kind that ARC reads, writes or
  reserves. A test checks it against the code and the manifests.
- docs/GRANDFATHERED.md, the list of the specs and packages that predate the
  rules. The list only shrinks.
- The pull request check "Spec policy". It refuses a change that edits a
  record of gates, that adds to the grandfathered list, or that changes
  `core/` or `sdk/` and does not name a spec.
- The `spec` skill, which runs the gate interview.

### Changed

- `arc tool` is now `arc apps`. `arc apps init` creates an app.
- The app programs are `arc-exec`, `arc-sqlite`, `arc-http` and
  `arc-releases`. They were `exec-provider`, `sqlite-provider`,
  `http-provider` and `releases-provider`. Update each `arc serve` command
  and each launch configuration.
- Each app lives in `apps/<name>/`: its manifest, its Arcfile, its service,
  and its program in `apps/<name>/cmd/arc-<name>`. The manifests moved from
  `manifests/`. `cmd/` holds only `arc`, and `arc` imports no app.
- The Go packages moved, and no package stays at its old path. An app imports
  only `sdk/`, so an app can live in another repository.
  `internal/architecture` checks this rule.

  | v0.16.0 | Now |
  | --- | --- |
  | `delivery/call`, `draft`, `keys`, `mail`, `node`, `private`, `relaylist`, `store`, `transport` | `core/<name>` |
  | `delivery/groups`, `limits`, `sealed` | `adapters/relay/<name>` |
  | `delivery/transport/file`, `delivery/transport/relay` | `adapters/transport/file`, `adapters/transport/relay` |
  | `delivery/catalog` | `runtime/catalog` |
  | `delivery/testrelay` | `internal/testrelay` |
  | `iface`, `capability`, `bundle`, `lists`, `release`, `wake` | `runtime/<name>` |
  | `provider` | `sdk/provider` |
  | `provider.HTTP(handler)` | `httpadapter.New(handler)`, from `sdk/httpadapter` |
  | `provider.Run` with the process streams | `stdio.Run` or `stdio.Main`, from `sdk/stdio`. `provider.Run` needs its input and output. |
  | `provider.ConfigPath`, `ReadConfig`, `Grants`, `Limit`, `Directory` | `sdk/providerconfig` |
  | `provider/host` | `adapters/provider/host` |
  | `store.Open(directory)` | `boltstore.Open(directory)`, from `adapters/store/bolt`. A core embedder uses `store.New`. |
  | `mail.Open(directory, ...)` | `mailbox.Open(directory, ...)`, from `adapters/mailbox`. A core embedder uses `mail.New` with a `core/kv` store. |
  | `keys.Save`, `Load`, `Read`, `Write` | `adapters/keyfile` |
  | `keys.ResolvePublic` | `nip05.ResolvePublic`, from `adapters/nip05`. `core/keys.ParsePublic` parses a key offline. |

- The end-to-end proofs are Go tests in `internal/proof`. `mise run delivery`
  and `mise run interface` run them.
- The release build check is `.github/scripts/check-clean-build.sh`.
- The proposals moved to `docs/proposals/`: the private environment contract,
  the Bluetooth plan, and the machine lifecycle definition.
- `docs/sqlite/SPEC.md` is now part of `apps/sqlite/README.md`. An app
  documents itself.
- Each spec has the same sections: purpose, terms, rules, behavior,
  failures, security, compatibility, proof, and gates. See
  docs/SPEC-TEMPLATE.md, section 9. `go test ./internal/specs` checks the
  headings. No rule of a spec changed.
- `docs/exec/SPEC.md` is now `docs/wake/SPEC.md`, the spec of the wake. The
  exec app documents its requests, lease and jobs in `apps/exec/README.md`.
- The interface spec no longer holds a section for each app, or a copy of
  each app manifest. Each app documents itself, and links its
  `manifest.json`. The dm, agora and files apps have a README.
- The guide `docs/sessions/PROVIDERS.md` is a section of `apps/README.md`.
  `docs/updates/OPERATIONS.md` is part of the updates spec.
  `docs/updates/PUBLISHING.md` is a section of `apps/releases/README.md`.

### Removed

- `docs/site/`, the illustrated website. It is now a repository of its own.
- `docs/WHITEPAPER.md` and `docs/WHITEPAPER_HUMAN.md`. They moved to the
  repository of the website.
- `docker/fly-nostr/`, the Fly.io files of one operator's relay. The image
  `ghcr.io/gezibash/arc` runs a relay on any host. `arc relay serve --help`
  lists the flags that turn on the write limits.
- `examples/`, with the notes examples. Tests keep their own programs in
  `testdata/`.
- The shell proofs in `scripts/`, and the tasks `mise run compose` and
  `mise run check-relay`.
- The start, lease and notify scripts of the exec app, in
  `cmd/exec-provider/citizen/` in v0.16.0. The operator supplies them.
  docs/wake/SPEC.md, sections 3.6 and 3.7, states what each one must do.
- The `arc-journal` skill. It named one operator's relay and journal.
- `docs/assets/arc-header.prompt.md`, the prompt of the header image.
- `docs/DEPLOY.md`. The README shows how to install `arc` and run a relay.
  `arc relay serve --help` lists the write limits.
- The notes on one hosting platform in the exec spec, and the measurements
  taken on it with the older stack.
- The relay `wss://arc-nostr-gezim.fly.dev` as the example relay in the
  documents. Each document now shows `wss://<relay>`.

### Fixed

- A canceled operation no longer crosses an adapter. Directory sends,
  remote signers, provider requests and HTTP handlers refuse canceled or
  expired work. A canceled HTTP request no longer returns an empty 200.
- Remote signers keep the deadline of the caller, with a cap of 20 seconds
  for each method.
- A temporary signer or store failure no longer turns a queued call into
  `outcome_unknown`. The call runs once after recovery.
- Incoming mail returns storage, signer and cancellation errors. It no
  longer returns an empty result.
- Blocked provider output no longer stops cancellation or shutdown. Output
  has one writer at a time, and a failed write stops the runtime.
- A queued request no longer stays uncertain when receipt storage stops
  early. The pending journal and the sealed request are stored first.
- Two session tests no longer fail at random under load.

## [0.16.0] - 2026-09-23

A provider can call the capabilities of its citizen, and can serve HTTP over
ARC. A Go provider with a `SetEvents` method does not compile. Remove the
method.

### Added

- A provider program can call a capability that its citizen installed. The
  program writes a `call` line. `arc serve` makes the call as the citizen,
  and writes a `result` line. The program never holds the key. A call to a
  capability that the citizen did not install fails with `not_installed`.
  In Go, a handler gets a `provider.Caller`. See docs/interface/SPEC.md,
  section 14.2.
- HTTP over ARC. `provider.HTTP` serves a Go `http.Handler` as a provider,
  and opens no port. The handler reads the key of the caller from the header
  field `Arc-Caller`. See docs/http/SPEC.md.
- `http-provider` serves an HTTP server of any language over ARC. It starts
  the server, and forwards each call to it. The server calls other
  capabilities through a local endpoint that takes only its token. See
  docs/http/SPEC.md, section 10.
- The examples `examples/notes` and `examples/notes-server`: one HTTP
  service that keeps its notes in SQLite over ARC. The first runs in the
  provider program. The second is a plain HTTP server with no ARC library,
  behind `http-provider`. `mise run compose` proves both end to end.
- `arc apps init` writes `interface.json` beside `manifest.json`. It is a
  manifest of interface version 1 with one command. After a caller installs
  the bundle, `arc <name> say <message>` sends the message to the runtime and
  shows the reply. Before, a caller could reach a starter bundle only with
  `arc call`. The id of the interface comes from the name of the directory,
  as the scheme does. If that name starts with a digit, the id and the
  scheme start with `app-`. They have at most 64 characters.

### Removed

- **Breaking:** the `Events` and `WantsEvents` interfaces of the `provider`
  package, and `Emit`. `arc serve` dropped each event line of a provider, so
  an event never reached its citizen. If a provider has a `SetEvents` method,
  remove it.
- The command line interfaces of the older stack, versions 1 to 4, in
  `manifest.json`. They do not run on the delivery layer. `arc serve` still
  loads a manifest that holds one, but the announcement leaves out
  `interfaces`, `cli`, `invocation.mode` and `invocation.stream`.
  `arc apps init` writes a manifest without a command line.

### Fixed

- `arc serve <bundle directory>` gives the program each argument of
  `runtime.args` in the Arcfile. Before, the program got the JSON text of
  the list, split at each space. For example, `args = ["-u", "server.py"]`
  gave one argument: `["-u","server.py"]`. The `args` parameter of an
  `exec://` address can now be a JSON list, and an argument in the list can
  contain a space. An address that gives the arguments with spaces between
  them, for example `args=-v+--x`, still works.
- `arc serve <bundle directory>` works when the path of the directory has a
  `#`, a `%`, or a `?` in it. Before, `arc serve` put the path of the program
  into the `exec://` address without escaping it. When `arc serve` read the
  address back, it cut the path at `#` or `?`, changed `%20` into a space, or
  refused the address. For example, `arc serve bot#1` failed with "serve:
  .../bot is not a program". An `exec://` address that you write yourself
  works as before.
- `arc serve <bundle directory>` runs the program in the directory that
  `runtime.cwd` in the Arcfile names. Before, the program ran in the
  directory of the shell. For example, with `args = ["-u", "server.py"]`,
  the program found `server.py` only if the owner started `arc serve` in the
  bundle directory. If the Arcfile has no `cwd`, the program runs in the
  bundle directory. `PWD` in the environment of the program names the same
  directory. The `exec://` address carries the directory in a new `cwd`
  parameter. The directory must exist. If it does not, `arc serve` stops
  with an error. A hand-written address without `cwd` works as before.
- `arc apps init <directory>` writes a valid manifest.json when the name of
  the directory starts with a letter that is not ASCII. Before, it cut that
  letter in half, so the file was not valid JSON, and `arc serve` refused
  it. For example, `émile-bot` gave `"title": "�\xa9mile Bot"`. Now the
  title is "Émile Bot". `arc apps init` also writes each string of
  manifest.json with a JSON encoder, so no directory name can make the file
  invalid JSON.

## [0.15.4] - 2026-09-23

### Fixed

- The binaries in the image and in the Fly.io relay image report a clean
  build. Before, `arc version` printed "commit <sha> (modified)" for them,
  because the build left out tracked files and git saw them as deleted.

## [0.15.3] - 2026-09-23

### Fixed

- The linux-aarch64 and darwin-aarch64 binaries of a release report a clean
  build. Before, `arc version` printed "commit <sha> (modified)" for them,
  because the release build saw its own tarballs in `dist/` as changes.

## [0.15.2] - 2026-09-23

### Fixed

- `arc serve` prints "serves" only when at least one relay has the watch for
  live calls. Before, a relay that refused its first watch also counted, so
  with no relay up, "serves" came and every live call failed with "mute: no
  one was listening for this". `arc serve` logs each relay that refused its
  first watch, tries it again every 3 seconds, and sends the announcement
  again. With no relay up, `arc serve` waits for one.
- `arc serve` sends the relay lists of NIP-65 and NIP-17 to a relay or an
  indexer that comes back after start-up. Before, a relay or an indexer that
  was down at start-up never got them. Then a caller that shares no relay
  with the provider could not find the provider. Each sync now sends the
  lists to the relays. Each sync also sends them to an indexer that did not
  get them at start-up, until that indexer has them.

## [0.15.1] - 2026-09-23

### Fixed

- `arc serve` prints "serves" only after each relay has taken the watch for
  live calls. Before, a first call just after that line could fail with
  "mute: no one was listening for this". A watch of a relay now waits for the
  end of stored events, for up to 15 seconds.

## [0.15.0] - 2026-09-23

A live call finds the relays of the provider through its NIP-65 relay list,
and an indexer relay can hold those lists. `arc call` uses the newest
announcement of the provider. No manifest changes, so machines can update in
any order.

### Added

- A citizen publishes its NIP-65 relay list, kind 10002, beside its other
  relay lists. `arc relay add`, `arc relay rm` and `arc serve` publish it.
- A live call also goes to the read relays of the provider's NIP-65 list, so
  a caller and a provider that share no relay can call. See
  `docs/delivery/SPEC.md`, section 11.4.
- `arc relay add <url> --index` names an indexer relay. `arc` publishes its
  NIP-65 and NIP-17 relay lists there, and looks up the lists of others
  there, for live calls and for direct messages. `arc` names no indexer by
  default. See `docs/delivery/SPEC.md`, section 7.2.

### Fixed

- `arc call` asks the relays for the newest announcement of the provider
  before each call, as an installed command does. Before, it used the
  announcement that this machine held, so a new manifest, for example the
  `service.output` of 0.14.0, took effect only after `arc sync`.

## [0.14.0] - 2026-09-23

`arc call` shows a reply as the manifest of the service says. Update each
caller to 0.14.0 before a provider of exec or sqlite serves the new manifest:
a caller of 0.13.0 refuses it.

### Added

- The service of a manifest can have `output`: the pipeline that shows the
  reply of `arc call`. `arc call` then shows the reply as the service says,
  and `--raw` writes it as it came. See `docs/interface/SPEC.md`, section 4.3.
- `arc call` of exec shows the output of the command, and exits with its
  code, or 75 while a job runs. `arc call` of sqlite shows a table.

### Changed

- **Breaking:** the manifests of exec and sqlite use `service.output`. A
  caller of 0.13.0 refuses a manifest with a field that it does not know.
  Update each caller before a provider serves the new manifest.

## [0.13.0] - 2026-09-23

### Added

- `arc call` takes an address again: `<scheme>+arc://<provider>/<path>`, as
  the older stack did. The scheme names the capability, the path is the path
  of the request, and the provider can now be a key, an npub, an installed
  name, or a domain for NIP-05. See `docs/interface/SPEC.md`, section 14.1.

## [0.12.1] - 2026-09-23

### Fixed

- A command whose output renders to nothing shows nothing. Before, it showed
  an empty line, for example `arc exec run true`.

## [0.12.0] - 2026-09-23

`arc exec run` exits with the exit code of the command again. Update each
caller to 0.12.0 before a provider of `exec` serves the new manifest: a
caller of 0.11.0 refuses it.

### Added

- The output of a manifest command can have `exit`: rules that set the exit
  status of the command from its first record. See `docs/interface/SPEC.md`,
  section 9.4.

### Changed

- **Breaking:** the manifest of `exec` uses `exit`. A caller of 0.11.0
  refuses a manifest with a field that it does not know. Update each caller
  before a provider serves the new manifest.

### Fixed

- `arc exec run` exits with the exit code of the command, as `arc-exec`
  did. `arc exec status` exits 75 while the job runs, and 1 when the job is
  lost. Before, both exited 0.

## [0.11.0] - 2026-09-23

The older stack is gone, and `arc` is the only program of ARC. This is step 5
of the switchover in `docs/delivery/SPEC.md`, section 15.1. The home of `arc`
moves to `~/.config/arc`: see "Upgrade from v0.10.0" in the README.

### Changed

- **Breaking:** the home of `arc` is `~/.config/arc`, where it was
  `~/.config/arc/next`. Move it one time, as the README says.
- The image `ghcr.io/gezibash/arc` runs `arc relay serve` on port 7447. It
  ran `arc-relay` on port 7331.
- The petnames of keys come from `delivery/keys`. The names stay the same.

### Removed

- **Breaking:** `arc-legacy` and `arc-relay`, and the packages of the older
  stack: `identity`, `sealedbox`, `session`, `packet`, `frame`,
  `internal/wire`, `announce`, `relay`, `client`, `direct`, `control`,
  `citizen`, `toolbox` and `relays`.
- The DM, journal, Agora and files providers. Their manifests in
  `manifests/` replace them, with no provider.
- `arc-exec`, the local Compose stack, `mise run cli`, and the docs of the
  older stack. Links to those docs point to the tag v0.10.0.
- A binary, `arcn`, that v0.10.0 committed by mistake.

## [0.10.0] - 2026-09-22

`arc` now runs the delivery layer on Nostr events, and the older program is
`arc-legacy`. This is step 4 of the switchover in `docs/delivery/SPEC.md`,
section 15.1. `arc` of 0.9.0 cannot update to this release: install it with
`install.sh`. Move each identity by hand: `arc keys gen` makes a new one, and
the older keys stay with `arc-legacy`.

### Added

- Before a live call, `arc` runs the wake hook of the provider from
  `<home>/wake.toml`. Without a hook, the provider needs an announcement
  that is at most 5 minutes old, or the call stops with `peer_offline`.
  `arc serve` signs its announcement again every 2 minutes.
- `arc lists add`, `rm` and `ls` save sets of citizens for one installed
  command. Where the command takes a key, the name of a list runs it once for
  each member.
- `arc update`, `update check` and `update apply` read a signed release
  channel from a releases provider with live calls, and replace the program.
- `arc release sign` checks the archives of a channel, signs it, and writes
  it for the releases provider.

### Changed

- **Breaking:** `arc` is the program of the delivery layer, built from
  `cmd/arc`. Up to 0.9.0 it was `arcn`. It reads `ARC_HOME`, `ARC_KEY` and
  `ARC_PASSPHRASE`, where `arcn` read `ARCN_HOME`, `ARCN_KEY` and
  `ARCN_PASSPHRASE`. Its home stays `~/.config/arc/next` until 0.11.0.
- **Breaking:** the older program is `arc-legacy`, built from
  `cmd/arc-legacy`. It writes a deprecation notice to standard error on each
  run, and 0.11.0 removes it. `arc-legacy` and `arc-relay` read the identity
  from `ARC_LEGACY_KEY`, where they read `ARC_KEY`. `install.sh` links only
  `arc`.
- The local Compose stack runs `arc-legacy`.

- **Breaking:** a release channel has schema 3. Its publisher is a Nostr key,
  and its signature is BIP-340 Schnorr. This build does not read schemas 1
  and 2, which used Ed25519. A machine on 0.9.0 installs the next release
  with `install.sh`.
- `release.ChunkBytes` is 64 KiB, so that one reply fits in an event of
  256 KiB on a relay.
- The citizen scripts of the exec provider run `arc serve`. `citizen/init`
  takes `--relay` as a Nostr relay URL, and no longer takes
  `--relay-pubkey`.

### Fixed

- `arc -v` prints the version, as `arc --version` does.
- Two commands of one identity run at once. A command holds the files of
  the identity only while it uses them, and lets them go 200 ms later.
  Before, a second command failed with `store: timeout` while `tail` or
  `serve` ran.

- `arc --version` prints the version. Before, it failed, and `arc update`
  could not check a new program.
- Two equal live calls in one second are both answered. Each request rumor
  carries a `nonce` tag. Before, the provider refused the second call as a
  replay, and the caller waited until its timeout.

### Removed

- `docker/fly/`, the image of the older relay and the DM provider on Fly.io.
  The Fly.io app `arc-relay-gezim` is stopped. The Nostr relay,
  `docker/fly-nostr/`, replaces it.

## [0.9.0] - 2026-09-22

`arcn` gets the identity, tool and info commands of `arc`, and a relay that
limits writes. The Fly.io app `arc-nostr-gezim` runs that relay. Update
`arcn` before you use that relay: an `arcn` of 0.8.0 cannot make a live call
through it, or send mail to an inbox on it.

### Added

- `arcn keys gen`, `add`, `list`, `use`, `remove`, `encrypt` and `bunker`
  manage identities. Each identity has its own directory,
  `<home>/citizens/<name>`. `--key <name>` and `ARCN_KEY` pick an identity
  for one command. `arcn whoami` shows the identity.
- `arcn tool list`, `info`, `remove`, `resolve`, `apps init` and `version`
  come from `arc`.
- `arcn relay serve` takes write limits. Each limit is off when its flag is
  absent. `--max-event-bytes` caps the size of one event. `--wrap-auth` takes
  a gift wrap only after NIP-42 authentication, and `--wrap-pow` lets a gift
  wrap with NIP-13 work in without it. `--rate` and `--burst` cap the events
  of one IP address, and `--ip-header` names the header that holds that
  address. `--max-store-mb` caps the store.
- `docker/fly-nostr/` runs `arcn relay serve` as the Fly.io app
  `arc-nostr-gezim`, with the write limits on. `mise run check-relay -- <url>`
  checks one running relay.
- A capability can carry bytes. With `encoding: base64` in `request_body` or
  `response_body`, the citizen passes that body to the runtime as base64,
  and reads it back the same way. No byte changes on the way.
- `arc serve` and `arc listen` take `--federate` or `--federate-network`.
  With `--federate`, the direct partners of the relay find the citizen. With
  `--federate-network`, the wider network finds it where transit allows. The
  two flags are mutually exclusive.
- `arc listen` announces the citizen when it connects, and again every 150
  seconds. `arc resolve` and `arc send` find a listener by name.
- A relay with partners asks them live when its catalog is cold, or when it
  does not hold a name or a public key. The lookup carries an id, a path and a
  budget of 64. Each relay runs at most 32 lookups and asks at most 16
  partners for each.

### Changed

- **Breaking:** `arcn key` is gone. `arcn keys add` replaces `arcn key use`,
  and `arcn whoami` replaces `arcn key show`. An `arcn` home keeps each
  identity in `<home>/citizens/<name>`.
- **Breaking:** `arc join` takes the pin with `--relay-pubkey`, like the
  other commands. `--pubkey` is gone.
- `arc join` shows the key and the petname of a new relay, and pins the key
  only after you type `yes`. It asks the relay with a temporary identity, so
  it never replaces the route of a running citizen. With no key and no
  selector, it makes the first identity of the machine. It warns when
  `ARC_RELAY` or `ARC_RELAY_PUBKEY` hides the relay that it saved.
- `arc status` asks the relay with a temporary identity. It needs no key,
  reads no selector, and never replaces the route of a running citizen.
- `arc-relay --generate` makes a key only when the store holds none, and
  makes it the default, so the next start finds the same key. With
  `--key NAME`, it refuses a name that the store does not hold.
- `arc call` writes the response body byte for byte. It adds a newline only
  when standard output is a terminal.
- A direct policy that does not match leaves `arc call` on the relay, with a
  note on standard error. Before, the call failed.
- A provider has 5 seconds to end after its input closes. Only then does the
  citizen kill it.
- A lookup by name or prefix that did not reach every partner fails with
  `federation_unavailable`. Before, it returned its entries with
  `partial: true`.

### Fixed

- The status of a relay that runs with `--transit` says
  `"federation_transit": true`. Before, it always said `false`.
- An empty `arc.key` or `default.key` fails the command. Before, `arc` went
  on to the next selector.
- A selector that names no key fails with the selector and the name, for
  example `ARC_KEY names "nope"`. Before, the error said `no identity`.
- `arc` reads the legacy `~/.config/arc/default_key` when `default.key` is
  absent, as 0.6.0 did. `arc keys use` removes it after it writes
  `default.key`.
- `arc whoami --key NAME` says `chosen by --key`.
- An unknown subcommand, such as `arc keys show`, fails with status 1.
  Before, `arc` printed the help and exited with status 0.
- Each error prints once, after `arc:`.
- Two joins at the same time keep both relays. A lock on
  `relays.json.lock` covers each read and write of `relays.json`.
- `arc serve` says why it stopped: `the relay connection ended`, or
  `the provider stopped`. Before, it said `signal: killed`. A stop with
  Ctrl-C exits with status 0.
- When the relay connection of `arc serve` ends, a direct route that stands
  serves until its lease ends.
- The citizen refuses a request id that already waits, and a request over
  256 that wait.
- `arc call` refuses an address path with a percent escape, a dot segment,
  or `/info`. It refuses a capability of another id or another mode than
  `request_reply`, and a body that is not UTF-8 for a text capability. The
  citizen refuses such a body too.
- The Agora board holds its directory with `flock`, so it starts again after
  a restart. Before, a restart could leave it at `storage_locked` for good.
- `Relay.Close` no longer waits for a client that joined while it closed.
- A direct route renews its lease over the relay while half of the lease
  remains. Before, a route never renewed its lease.
- When the lease of a direct route ends, both sides close the carrier, and the
  conversation returns to the relay. Before, both sides kept the carrier open
  after the lease ended.
- A live call of `arcn` answers the NIP-42 challenge of a relay, and sends
  again. Mail of `arcn` answers the challenge of the inbox relay of a
  recipient with a one-time key.
- `arcn` keeps the request of a relay for authentication when a query
  closes. Before, a read of a sealed part could fail at random.
- When no relay answers, `arcn` says so. Before, it said that an event was
  missing, and gave the wrong advice.

### Known issues

- `arc` refuses the `private_file`, `sealed_file` and `agora` inputs, so
  `arc files` and `arc agora` fail.

## [0.8.0] - 2026-09-21

`arc` wakes a citizen whose machine pauses, and it says at once when a
citizen is not there. This completes phase 2 of `docs/exec/SPEC.md`. Only the
caller needs this version. A citizen on 0.7.0 works with it.

### Added

- `arc` wakes a citizen whose machine pauses. Before a request to the
  citizen, it runs the wake hook of the citizen from `wake.toml` in the
  directory of ARC. After an answer, it skips the hook for 30 seconds.
- A hook that exits with a status other than 0 stops the request with
  `wake_failed`. A hook that does not end in 30 seconds, or before the
  deadline of the request, stops it with `wake_timeout`.
- A hook that this `arc` cannot run, for example of a kind other than
  `command`, fails only the requests to its citizen. A `wake.toml` that is
  not valid fails every request, and no other command.
- A request to a citizen without a wake hook needs a current announcement of
  the citizen on the relay. Without one, the request stops at once with
  `peer_offline`. Before, it waited for the timeout.
- `arc resolve` shows the presence state of each citizen: `online`,
  `asleep` (no announcement, and a wake hook), or `offline`. It finds a
  citizen that sleeps by the name or the key of its wake hook.
- The `wake` package runs the wake flow. The `client` package takes a waker
  as `Options.Waker`, so every request of a library user follows the flow,
  and `Peers.Online` asks the relay about one citizen.

### Changed

- `arc-exec` leaves the wake to `arc`. It no longer reads `wake.toml` or
  `ARC_WAKE_CONFIG`, and it needs Python 3.9 or newer instead of 3.11.

### Removed

- The traversal lab, its results, and the tests and tools that needed the
  Elixir implementation. The results stay at the `v0.5.2` tag.

### Fixed

- The relay of the Fly.io image reports its release version. Build the image
  with `--build-arg VERSION=X.Y.Z`. Before, the relay reported `dev`.

### Known issues

- `arc` refuses the `private_file`, `sealed_file` and `agora` inputs, so
  `arc files` and `arc agora` fail.

## [0.7.0] - 2026-09-21

ARC is a Go module. A release holds one static binary for each command, and a
citizen machine needs no language runtime. `arc` runs the relay protocol of
earlier versions. `arcn` runs the delivery layer on Nostr events.

### Changed

- **Breaking:** Go replaces the Elixir implementation. The release tarballs
  and the image hold Go binaries.
- **Breaking:** the relay is the binary `arc-relay`. Start it with
  `arc-relay --key NAME --address :7331`. The image runs `arc-relay` by
  default. To run another command in the image, name it, for example
  `docker run IMAGE arc keys gen`.
- **Breaking:** `arc call ADDRESS [BODY]` replaces
  `arc request ADDRESS --body BODY`. `--timeout` counts seconds, and the
  default is 30. The body comes from the second argument or from standard
  input. `--input`, `--output` and `--local` are gone.
- **Breaking:** `arc status` takes `--json` only. `--format` and `--check`
  are gone. An error goes to standard error, and the JSON has no `address`
  field.
- **Breaking:** `arc install` takes `--yes` in place of `--trust`.
- **Breaking:** `arc update` reads the channel from the provider that
  `--provider` or `ARC_RELEASES` names. It does not search the relay.
  `arc update status`, `--source` and `--replace-publisher` are gone.
- `arc send` returns when the message leaves for the relay. `--wait` waits
  for an answer.
- `arc keys list` marks the global default with `*`, not the active
  identity.
- **Breaking:** `arc whoami` replaces `arc keys show`. `arc keys gen` prints
  the name and the public key on two lines. `arc keys list` and
  `arc keys remove` replace `arc keys ls` and `arc keys rm`.
- The exec citizen runs the `exec-provider` binary of a release, so the
  machine needs no Go toolchain. `citizen/init` finds the binary next to `arc`
  and writes `EXEC_PROVIDER` to `citizen.env`. `arc-exec` sends each request
  with `arc call`.
- `citizen/citizen-up` looks for its own announcement in up to 50 exec
  citizens. Before, it looked in 10.

### Added

- The delivery layer, in `delivery/` and the `arcn` command. Every datum is a
  signed Nostr event. A relay or a directory, such as a USB stick, carries the
  events. See `docs/delivery/SPEC.md`.
- Version 1 of the capability interface. A capability is a JSON manifest of
  primitives. Seven providers use it: exec, sqlite, releases, journal, dm,
  agora and files. See `docs/interface/SPEC.md`.

### Removed

- **Breaking:** the MCP server, `arc mcp` and `arc mount`.
- **Breaking:** the hot upgrade of a running relay. To update a relay, replace
  the binary and restart the relay.
- **Breaking:** `arc host`.
- **Breaking:** `arc tool update` and `arc apps open`.
- The TCP hole punch of direct connections.

### Fixed

- Two races made the direct route and federation tests fail under load. A
  control message now always travels on the relay. A lock for each federation
  link covers the encryption and the write. A relay drops the link of a
  partner connection that ended.
- The image, the Fly.io entrypoint, the local Compose stack and the exec
  citizen scripts called `arc relay`, `arc keys show` or `arc request`. The
  Go `arc` does not have these commands. The local relay health check used
  the Erlang runtime.
- The image reports its release version. Before, it reported `dev`.

### Known issues

- `arc` refuses the `private_file`, `sealed_file` and `agora` inputs, so
  `arc files` and `arc agora` fail.

## [0.6.0] - 2026-09-20

No release was published for this version.

A relay and its clients must run the same version. This release changes the
relay announcement and the key exchange, so a mixed network does not work.

### Changed

- **Breaking:** the X25519 key of an identity now uses the standard
  conversion from Ed25519, the same as libsodium. Data that an earlier
  version sealed does not open. This includes DMs, private files, and the
  local sealed cache.
- A sender can now seal to any public key without a published key exchange
  record. `seal:to` computes the recipient key from the Ed25519 public key.
  A DM to a citizen on a different host no longer needs a shared control
  directory.
- Sessions compute the X25519 key of the peer from its Ed25519 public key.

### Removed

- **Breaking:** the key exchange record. `Control.publish_keyex/2` and the
  `x25519_public` field of control plane entries and relay announcements are
  gone. `arc publish` and `arc resolve` print no `keyex` or `x25519` lines.
  Relays and clients of this version reject announcements from earlier
  versions, and the reverse. Upgrade them together.
- The `has_key_exchange` field of the host `resolve` operation.

### Added

- `Arc.Identity.public_key_to_x25519/1` computes the X25519 public key of an
  identity from its Ed25519 public key.
- The exec provider at `providers/exec`. A granted key runs commands on the
  machine of a citizen through the relay. A job continues after the caller
  disconnects, and the provider reports the result to the caller. See
  `docs/exec/SPEC.md`.
- A citizen machine that pauses when it is idle. The caller wakes it, and the
  citizen holds a lease while it works. `providers/exec/citizen/` holds one
  script for each platform.
- `docs/exec/MLD.md`, the machine lifecycle definition. It describes the
  contract for a new platform, and the delegated identity of a per-task
  machine.
- `docker/fly/`, an image that runs a relay and the DM provider on one Fly.io
  machine.

## [0.5.2] - 2026-09-19

### Fixed

- A schema 2 channel can again announce a restart-only release without an
  `install` object if the release sets `eligible` to false. Channels published
  before `install` existed use this form. Since v0.5.0 such channels failed
  verification with `invalid_sources`, and a publisher could not add a new
  sequence to them. v0.5.0 and v0.5.1 still reject these channels.

## [0.5.1] - 2026-09-19

A managed relay must use a restart-only update for this release, because it
changes modules other than `Arc.Net.Relay`.

### Fixed

- One relay client could disconnect other clients. A packet header that was not
  a JSON object, or a header field that was not a string, crashed the
  relay-side connection before the signature check. The crash also stopped the
  acceptor and every connection that it accepted. The relay now drops such a
  packet, and connections no longer link to their acceptor.
- Relay-side connections now close when their relay stops, so clients
  reconnect to a restarted relay.
- A restarted route shard keeps the routes of live clients. A registration for
  a stopped shard now goes to its replacement.
- The relay traps exits and keeps its route shards linked, so the shards stop
  with the relay. A new relay also stops shards that a previous relay left.
- In the MCP HTTP server, a session that stops no longer stops the server or
  the other sessions. Its agent stops, and later requests get 404, so the
  client starts a new session with the same key.
- A second MCP `initialize` with a key that is already active gets 409. Before,
  it stopped the server.
- MCP session servers no longer restart after DELETE, so four fast deletes no
  longer stop the server.
- MCP calls that take too long get an HTTP error, not a closed socket. The
  `send` tool finishes in 20 seconds.
- The MCP server writes a missing JSON-RPC id as `null`, not `"nil"`.
- The MCP acceptor tries again after a temporary accept error, for example when
  file descriptors run out.

## [0.5.0] - 2026-09-18

### Added

- `arc update` without `--socket` updates the local installation through the
  configured relay as the active citizen key: it discovers a `releases`
  provider (or uses `--source`),
  verifies the signed channel against a remembered `--publisher` key with
  replay protection, downloads the newest eligible complete archive in
  bounded chunks, checks its digest and tar members, starts the candidate to
  confirm its version, and swaps it into place, keeping the replaced release
  as `<root>.previous`. `check` reports only; `status` reads local state.
  `--channel`, `--replace-publisher`, `--relay`, `--relay-pubkey` and
  `--format json` are supported.
- Channel documents accept an optional per-release `install` object naming
  the complete installation archive. Verifiers from earlier releases reject
  documents that carry it.
- `Arc.CLI.Update.Publisher` and `scripts/publish-release-channel.exs` sign and
  publish a channel document from an operator keystore key. Publication holds
  a root lock, requires an increasing sequence, keeps published builds
  immutable, checks the size and SHA-256 of every archive and `install`
  archive, and replaces the channel file atomically.
- Channel schema version 2 has the signature domain `ARC-RELEASE-CHANNEL-V2`.
  A version 2 release can have no hot-upgrade sources. It must then set
  `restart_required` and carry an `install` object that repeats its `sha256`
  and `size`, so `arc update` can install it. Releases through v0.4.1 reject
  version 2 documents.
- `scripts/verify-release-channel.exs` verifies a channel and downloads every
  archive through a pinned relay. `docker/local/compose.releases.yaml` adds an
  opt-in local releases provider. See `docs/updates/PUBLISHING.md`.

## [0.4.1] - 2026-09-17

### Added

- `arc join HOST[:PORT]` confirms and remembers a relay fingerprint, selects
  that relay for future commands, and creates an identity on a fresh setup.
  Known relay keys cannot be silently replaced. Explicit flags and environment
  settings retain precedence over saved relay defaults.

## [0.4.0] - 2026-09-17

### Added

- Experimental managed relay boot, a protected local `arc update` interface,
  signed release-channel verification, bounded ARC/local artifact transfer,
  conservative OTP package preflight, and a read-only release provider.
  Checks notify; updates require an operator. Production qualification and
  official signed channels are still required before rollout.
- An isolated application hot-upgrade/downgrade proof that passes encrypted
  traffic across existing relay connections while state migration is paused.
  CI runs the proof. This does not provide a production updater.
- A live-update design covering stable/beta channels and operator-started
  updates, with incompatible releases left pending for a restart decision.

### Changed

- Failed updates retain release identities and the interrupted phase in the
  private recovery journal. Unreadable recovery state blocks retries, and a
  rejected archive digest no longer leaves its temporary download behind.
- Moved the relay accept loop into its own module so it does not retain old
  relay callback code during the hot-upgrade proof.
- `arc status` now queries the configured relay by default, with service-owned
  fields, a key/value table, and `--format json` (`--json` alias). The previous
  combined snapshot JSON changes; use `arc host status` for the local host.
  Older relays need an upgrade for this query.
- Removed Docker inspection and the hardcoded local Compose project from
  `arc status`. Status no longer depends on the deployment platform.

### Fixed

- Background channel checks preserve failed-update status until an operator
  explicitly checks or retries, so a rejected download remains visible.
- Federation test shutdown uses one fixed deadline and optional stack-only
  diagnostics; shutdown also verifies cancellation of an outbound handshake.

## [0.3.2] - 2026-09-16

### Added

- `arc status` shows the selected identity, client relay configuration, live
  local host connections, and the local Compose stack. `--check` performs a
  bounded relay greeting check without registering a citizen; `--json` provides
  machine-readable output.

## [0.3.1] - 2026-09-16

### Added

- Identity selection through `ARC_KEY`, then `arc.key` in the current working
  directory, then `~/.config/arc/default.key`. Selector files contain existing
  identity names; invalid explicit selections fail without falling through.
- A local Docker Compose stack with a relay, journal, sealed DMs, and Agora,
  separate persistent identities and data volumes, and a public connection
  settings helper for installed clients.

### Changed

- `arc keys show` and `arc keys ls` report the effective identity and its source.
  `arc keys use` saves the global default to `default.key`. Existing `default_key`
  files remain readable until a new default is saved.

### Fixed

- Identity-selection errors now identify invalid or unreadable selector files.
  Failed legacy-selector cleanup reports partial success without crashing.
- Local provider containers retain their identities when upgrading the default
  selector format and use ARC's bundled runtime for their provider programs.

## [0.3.0] - 2026-09-16

### Added

- Relay-backed discovery with signed provider announcements, approved relay
  federation partners, opt-in onward traffic, and temporary cached catalogs.
- A private file-storage provider and client-side encryption, plus Agora's
  signed public discussions and local browser interface using the owner's ARC
  identity. Provider runtimes remain separately configured programs.
- `arc request <scheme+arc://provider-key/resource>` for bounded, opaque
  request/reply traffic through a pinned relay, or explicit local delivery.
  The SQLite provider supplies named databases and per-citizen grants.
- Owner-approved direct request/reply routes using mutual TLS, exact peer and
  resource policies, and finite permissions renewed through the relay. Either
  endpoint can accept the connection; both owners can opt into TCP hole punching.
- A reproducible cloud traversal lab with recorded successful direct routes,
  relay fallback, permission expiry, and interrupted-write behavior. See
  [verification results](https://github.com/gezibash/arc/blob/v0.5.2/docs/transport/VERIFICATION-2026-09-16.md). Home and
  mobile networks remain unverified.

### Changed

- Relay-backed citizens use their chosen relay even for nearby peers. Failed
  direct upgrades retain relay delivery. Application operations with unknown
  outcomes are never automatically replayed.
- The project website now uses full-screen parchment illustrations with
  accessible, restrained artwork motion.

### Fixed

- Strict lint and type-analysis findings across relay, provider, and local
  browser handling while preserving consent and message-delivery boundaries.

## [0.2.1] - 2026-09-15

### Added

- Installable releases: a mix release with the bundled Erlang runtime, one
  tarball per platform (linux-x86_64, linux-aarch64, darwin-aarch64), and a
  relay image on ghcr.io. A `vX.Y.Z` tag builds and publishes them. See
  `docs/DEPLOY.md`.

### Fixed

- `arc relay` no longer crashes in a release for lack of `:crypto`, and no
  longer prints an EXIT trace on SIGTERM.

- `arc dm fetch` rejects a blob name that could leave the message's blob
  directory. Attachment names never start with a dot.
- A `send` that fails part way, receipts included, removes every copy that
  landed and rebuilds each counter from disk.
- The mailbox byte counter rebuilds itself from a walk when the `usage`
  file is missing or malformed, never goes negative, and subtracts only
  bytes that actually left the disk.
- `retract` completes when a recipient already purged their copy, and emits
  `dm.retracted`.
- `thread` takes a conversation key as `conversations` prints it. A sender
  who lists themselves in `--to` keeps a plain one-to-one key.
- Events carry `to`, the conversation key the recipient can open.

## [0.2.0] - 2026-09-14

### Added

- `Arc.Identity.SealedBox`: seal a body to an X25519 public key with an
  ephemeral key, HKDF-SHA256, and ChaCha20-Poly1305. No session needed.
- `Arc.Identity.HKDF`: one RFC 5869 implementation for every derivation.
- Template filters `{{key|pubkey}}` and `{{key|seal:to}}`, a `seal_to`
  option on stdin inputs, and an `open` output filter that decrypts
  `sealed-v1:` tokens in a reply before the CLI prints it.
- `arc resolve` prints the peer's X25519 key. `arc publish` publishes it.
- `providers/dm`: sealed direct messages with a persistent inbox, receipts,
  threads, and a block list. Spec in `docs/dm/SPEC.md`.
- `arc version`.
- This changelog.

### Changed

- Session v2. The initiator derives the session key from a per-session
  ephemeral X25519 key, carried in the signed packet header as `ek`. This
  gives forward secrecy against compromise of the initiator's long-term
  key. The whitepaper now states exactly that.
- The replay guard evicts entries older than twice the clock skew window.

### Fixed

- A second `arc publish` no longer clears a published X25519 key.
- The test suite no longer writes to the real `~/.config/arc`.

### Deprecated

- Session v1 packets, those without `ek`, are still accepted and logged.
  The next release removes them.

## [0.1.0] - 2026-09-11

Initial release: identities, control plane, relay, exec providers, the
journal provider, MCP bridge.
