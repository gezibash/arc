# Transfer app

The transfer app gives a file to a citizen on a direct connection. It can
also take a file that a citizen gives to it. ARC carries the request and the
answer. The bytes do not go through a relay, so a file has no size limit.
If no direct path opens, the bytes go through the TURN server of the relay,
as the last resort, and the command says so.

The [manifest](manifest.json) declares the service. `server/` answers the
requests, `client/` gets a file, and `files/` holds what the two ends share.
The pipe between the two programs is the package `sdk/direct`.
[The direct pipe spec](../../docs/direct/SPEC.md) defines its rules: the
settings of each end, the order of the attempts, the relay attempt and the
flow control.
`cmd/arc-transfer` is the program: the service, and the commands `send`,
`offer`, `get` and `put`.

| Part | Status |
| --- | --- |
| `send`, `offer`, `get` and the service | Built. Tested with the built `arc`, a local relay and three citizens, and by hand between two machines behind NATs, with the STUN server of Google. |
| `put`, and `get` of a file that came with `put` | Built. Tested with the built `arc`, a local relay and two citizens, and by hand from a phone and a Mac to a virtual machine behind NATs. |
| STUN on the relay, the default STUN server | Built. Tested with a binding request to a local `arc relay serve`. Not tested between two machines behind NATs. |
| TURN on the relay, for two machines that find no direct path | Built. Tested on an emulated network, with two ends behind symmetric NATs and the TURN server of `arc relay serve`. Tested by hand on 2026-10-11 between two virtual machines behind NATs, through the relay on Fly.io; see "Speed". |
| A message that carries a link | Not built. Send the link as text, for example with `arc message send`. |

## How it works

1. The sender records a file as an offer, and gets a link.
2. The sender gives the link to the receiver.
3. The receiver makes a WebRTC offer, and sends it to the app of the sender
   in one ARC call. ARC verifies the key of the receiver.
4. The app replies with a WebRTC answer, and the two programs open a data
   channel between them.
5. The sender writes the bytes of the file on the data channel. The receiver
   writes them to `<file>.part`.
6. The receiver checks the SHA-256 of all the bytes against the link. Then it
   renames the part file.

The data channel has the encryption of WebRTC (DTLS). The ARC call carries
the certificate fingerprints of the two ends, so each end knows the other.

The app needs these things:

- A relay in the arc home of each citizen.
- A STUN server that each machine can reach. The default is the first relay
  of the citizen, on UDP port 3478: `arc relay serve` answers STUN there. For
  the relay `wss://relay.example.org`, the default is
  `stun:relay.example.org:3478`. The STUN server sees the public address of
  each machine. The two machines see the public address of each other. If the
  citizen has no relay, an end uses the addresses of its machine only.
- The two machines online at the same time.
- For the last resort, a relay that serves TURN (`arc relay serve
  --turn-ports`). `get` and `put` get a credential with `arc relay turn`,
  and the relay counts the bytes against a quota for each day.

## Use it

Build with `mise run build`, and use `bin/arc` and `bin/arc-transfer`.

### Send in one command

`send` does the work of the sender in one command, with no service that
runs before:

```sh
arc-transfer send -m "look at this" <key of the receiver> photo.png
```

1. `send` records an offer for each file, for this receiver only.
2. It runs `arc serve` with the transfer app, for this receiver only.
3. It sends one message: the text of `-m`, and one link on each next line.
4. It waits until the receiver has each file, and prints the path of each
   file that arrived. Then it stops its service.

If the receiver does not get each file in the time of `-wait`, 10 minutes
by default, `send` stops with an error. The message stays, but the links
work only while a service of the sender runs: run `send` again, or serve
the app.

Do not run `send` while a service of the same citizen runs for the app.
Two services then answer one request.

The receiver installs the app of the sender one time, see "The receiver".
The receiver can do this only after the sender served the app one time,
because the service announces the app.

### The sender

Serve the app. Keep it running. `arc serve` prints the key of the sender.

```sh
arc serve apps/transfer
```

Record a file. The command prints the link.

```sh
arc-transfer offer photo.png
```

```text
transfer+arc://<key of the sender>/<sha256>?name=photo.png&size=1048576
```

To give the file to one citizen only, name its key. Give `-to` one time for
each citizen.

```sh
arc-transfer offer -to <key of the receiver> photo.png
```

### The receiver

Install the app of the sender one time. For a second sender, give the app
another name with `--as`.

```sh
arc install <key of the sender> transfer
```

Get the file of a link. Put the link in quotation marks.

```sh
arc-transfer get 'transfer+arc://<key>/<sha256>?name=photo.png&size=1048576'
```

`get` prints the path of the file on standard output, and the speed on
standard error:

```text
photo.png
1048576 bytes in 2.0 s, 0.50 MiB/s, path srflx to srflx
```

`host` in the path means that the two machines are on one network. `srflx`
means that the path goes through a NAT. `relay` means that the bytes went
through the TURN server of the relay.

If a transfer stops, run the same command again. `get` reads the part file,
and gets only the rest.

### Give a file to the app of a citizen

A sender that runs no service can give a file with `put`. A phone does
this: the phone is open while the person sends, and it cannot answer a call
later. The receiver must serve the app, and must take puts.

The receiver sets the largest file that it takes, and serves the app:

```sh
TRANSFER_PUT_MAX_MIB=100 arc serve apps/transfer
```

The sender installs the app of the receiver one time, and gives the file.
`put` prints the link of the file:

```sh
arc install <key of the receiver> transfer
arc-transfer put <key of the receiver> photo.jpg
```

```text
transfer+arc://<key of the sender>/<sha256>?name=photo.jpg&size=1048576
```

The sender then sends the link in a message. When the receiver runs
`arc-transfer get` with the link, `get` finds the file in the state
directory, and makes no connection. A program that gets files with `get`,
such as the files adapter of gezibash/arc-harness, therefore needs no
change.

If a put stops, run the same command again. The app keeps a part file, and
`put` gives only the rest. If the app has all of the file, `put` gives
nothing.

## Settings

The service reads its settings from the environment. `arc serve` gives its
environment to the program.

| Variable | Value |
| --- | --- |
| `TRANSFER_STATE` | The directory of the offers. The default is `~/.local/state/arc-transfer`. `offer` and the service must use the same directory. |
| `TRANSFER_STUN` | The STUN server, or `none`. If it is not set, the service asks the first relay of `ARC_RELAYS` on port 3478. `get` and `put` read it too. |
| `TRANSFER_MIN_WINDOW_KIB` | The send window stays at or above this size after a loss. This makes a transfer faster on a path with losses. It is not fair to other traffic. |
| `TRANSFER_LOOPBACK` | If set, an end also uses the loopback address. The tests set it, because their two ends are on one machine. `get` and `put` read it too. |
| `TRANSFER_PUT_MAX_MIB` | The largest file that a caller can give with `put`, in MiB. If it is not set or 0, the app refuses each put. |

The commands take these flags:

| Command | Flag | Value |
| --- | --- | --- |
| `send`, `offer`, `get`, `put` | `-arc <program>` | The arc program. The default is `arc` on `PATH`. |
| `send`, `offer`, `get`, `put` | `-home <directory>` | The arc home. The default is the home that arc picks. |
| `send`, `offer`, `get` | `-state <directory>` | The state directory. The default is `TRANSFER_STATE`. `get` looks there for a file that came with `put`. |
| `send` | `-m <text>` | The text of the message, before the links. |
| `send` | `-wait <duration>` | How long `send` serves the files and waits for the receiver. The default is `10m`. |
| `offer` | `-to <key>` | A public key that can get the file. |
| `get` | `-o <file>` | The file to write. The default is the name in the link, in the current directory. |
| `get`, `put` | `-stun <url>` | The STUN server, or `none`. The default is `TRANSFER_STUN`. If it is not set, the command asks the first relay of `arc relay ls` on port 3478. |
| `get`, `put` | `-answerer-first` | Let the other end send first in the first attempt, and ask it to wait in the second. See "The order of the attempts". |
| `get`, `put` | `-no-relay` | Do not try the TURN server of the relay. If no direct path opens, the command fails. |

## Speed

The pipe grows the SCTP send window by 8 KiB for each round trip after a
loss, not by one packet, as pion does. The
[direct pipe spec](../../docs/direct/SPEC.md), sections 3.3 and 8.2, gives
the reason and the measurements on an emulated link.
`TRANSFER_MIN_WINDOW_KIB=512` gave 1.1 Mbit/s with a 64 KiB queue, and
84 Mbit/s with a 512 KiB queue.

On 2026-10-05, a real link from a Mac to a cloud VM, both behind NAT, gave
10 to 19 Mbit/s with the defaults of pion.

On 2026-10-11, a real link measured the step of the app. A boat.dev VM
(Ubuntu 24.04, x86_64) sent a file of 256 MiB to a Mac at home. Both were
behind NAT, and each path was `srflx to srflx`. The offer and the answer
went through `wss://arc-nostr-gezim.fly.dev`, with the STUN server of
Google. Only the sender changed between runs: `arc-transfer` at 74b1bb8
(step of pion), or at 422d0ed (step of the app). There were six runs of
each, and the last six alternated.

| Sender | Median | Mean | Range |
| --- | --- | --- | --- |
| Step of pion | 65.9 Mbit/s | 69.6 Mbit/s | 49.6 to 94.5 Mbit/s |
| Step of the app | 97.0 Mbit/s | 95.4 Mbit/s | 72.0 to 110.7 Mbit/s |

The median is 1.47 times higher. The ranges overlap, so one run does not
show the difference. The download rate of the Mac was not measured, so the
share of the link is not known.

The sender ran `arc serve` with `TRANSFER_STUN` set, and the receiver ran:

```sh
arc-transfer get -stun stun:stun.l.google.com:19302 -o out.bin '<link>'
```

A test by hand on 2026-10-11 sent 64 MiB through the TURN server of the
relay `arc-nostr-gezim` on Fly.io (one shared CPU, 256 MB). The receiver was
a virtual machine with a firewall that let out UDP only to the relay, so no
direct path opened. The time in all includes the two direct attempts, about
28 s. The time on the relay is from the ICE state `Connected` to the end, in
the debug log of pion, to the second.

| Sender | In all | On the relay | Speed on the relay |
| --- | --- | --- | --- |
| Virtual machine behind a NAT | 34.2 s | 6 s | about 89 Mbit/s |
| Same, with no STUN server (`TRANSFER_STUN=none`) | 35.1 s | 6 s | about 89 Mbit/s |
| Mac on a home network | 79.7 s | 51 s | about 10.5 Mbit/s |

The upload rate of the home network was not measured.

## Request protocol

A request is a live call to the capability `transfer`, with the method `RAW`
and the path `/`. The body and the reply are JSON.

```json
{"v":1,"sha256":"<64 hex>","offset":0,"hold_ms":0,"sdp":{"type":"offer","sdp":"..."}}
```

```json
{"v":1,"sdp":{"type":"answer","sdp":"..."}}
```

- `offset` is the number of bytes that the receiver has.
- `hold_ms` is the time that the sender waits before its first packet, at
  most 3000.
- `stun` is in a request through the TURN server of a relay only. It is a
  STUN URL with an IPv4 address and a port. The app adds it to its own STUN
  server. An app refuses a request with another form of `stun` as
  `invalid_request`. An app of an older version ignores the field.
- Each description holds all the addresses of its end. The app does not
  send addresses later.

The receiver opens one data channel. The sender writes the bytes from
`offset` in messages of 16 KiB, and then the text `end`. The receiver
checks the size and the SHA-256 of all the bytes, and renames its part
file. Then it answers with the text `ok`, or with `refused: ` and the
reason. A receiver that has the whole file already asks with `offset` equal
to the size, so that the sender sends no bytes and the receiver confirms
again. An older receiver answers `ok` before its check.

A put has the operation `put` and the size of the file. It has no offset:

```json
{"v":1,"op":"put","sha256":"<64 hex>","offset":0,"size":1048576,"hold_ms":0,"sdp":{"type":"offer","sdp":"..."}}
```

```json
{"v":1,"offset":524288,"sdp":{"type":"answer","sdp":"..."}}
```

- In the answer, `offset` is the number of bytes that the app has. If it is
  the size of the file, the answer has no `sdp`, and no bytes go.
- The caller opens one data channel. It writes the bytes from `offset` in
  messages of 16 KiB, and then the text `end`.
- The app checks the size and the SHA-256 of all the bytes. Then it answers
  with the text `ok`, or with the reason for a refusal.

A refused request has one of these errors:

| Error | Cause |
| --- | --- |
| `invalid_request` | The body is not a request of version 1. |
| `unknown_offer` | The sender has no offer with this SHA-256 for this caller. |
| `file_changed` | The size or the modification time of the file changed after the offer. |
| `busy` | The sender runs 4 transfers. |
| `transfer_failed` | The sender could not open its end of the connection. |
| `put_refused` | The app takes no puts: `TRANSFER_PUT_MAX_MIB` is not set. |
| `too_large` | The file of a put is larger than `TRANSFER_PUT_MAX_MIB`. |

## Rules of the sender

- A caller gets a file only if an offer for it is in the state directory. If
  the offer names keys, the caller must have one of them. A caller with
  another key gets `unknown_offer`, the same answer as for no offer.
- The sender refuses a file that changed after the offer. Record the file
  again.
- To limit the callers of the app, set `allow` in the `Arcfile`.
- An offer stays until you delete its file `offers/<sha256>.json` in the
  state directory.
- When a receiver answers `ok`, the service writes the file
  `delivered/<sha256>-<key of the receiver>` in the state directory. It
  holds the time and the path. `send` reads it. A refusal writes nothing,
  and the log of the service gives the reason.

## Rules of the receiver

- `get` does not replace a file. If the output file is there with the
  SHA-256 of the link, `get` tells the sender that it arrived, with no
  bytes: this recovers an `ok` that the network lost. Another file at the
  output stops `get`.
- The name in a link comes from another citizen. `get` uses only its last
  part, with no directory. It removes each character that could change the
  terminal or hide text, and newlines and tabs. If only removed characters
  stood before the first dot, the name gets the first 16 hex digits of the
  SHA-256 in front, so `<U+200B>.tar.gz` does not become the dot file
  `.tar.gz`. A dot file that the sender really named, such as `.bashrc` or
  `.tar.gz`, stays a dot file. A name that starts with `-` gets `_` in
  front.
- If the bytes do not have the SHA-256 of the link, `get` removes the part
  file, and writes no file.
- If the sender writes more bytes than the size in the link, `get` does not
  write them, removes the part file, and writes no file.

## Rules of a put

- The app takes a put only if `TRANSFER_PUT_MAX_MIB` is set. Each caller
  that reaches the app can then give files up to that size. To limit the
  callers, set `allow` in the `Arcfile`.
- The app writes the file to `received/<key of the caller>/<sha256>` in the
  state directory, and the part file next to it.
- If the caller writes more bytes than the size of the put, the app removes
  the part file.
- If the bytes do not have the SHA-256 of the put, the app removes the part
  file, and answers with the reason.
- `get` takes a file from `received/` only if the key and the SHA-256 of the
  link name it, and the size is the size of the link. `get` moves the file
  out of the state directory.

## The order of the attempts

`get` and `put` make the attempts of the
[direct pipe spec](../../docs/direct/SPEC.md), sections 3.5 and 3.6: two
direct attempts in each order of the first packets, then an attempt
through the TURN server of the first relay. The receiver of a `get` and the
caller of a `put` make the offer. In the first attempt, the other end
waits, so the end that makes the offer sends first. With `-answerer-first`,
the other end sends first in the first attempt, and waits in the second.
With `-no-relay`, the command makes no relay attempt.

In a relay attempt, the path is `relay`, and `send` prints it for each file
too. If the relay serves no TURN, or the attempt finds no path, the command
stops with `no direct path, and no path through the relay`. If the citizen
uses its quota during the transfer, the relay stops relaying. Run the
command again on the next day: it gets only the rest.

The TURN server is the relay of the citizen who runs `get` or `put`. Its
operator pays for the bandwidth, and sets the quota. See the delivery spec,
section 4.12.

## Tests

```sh
go test ./apps/transfer/...
```

The tests build `arc` and `arc-transfer`, and run them as a person does: a
relay on this machine, three citizens, and `arc serve`. Each test fails when
the behavior that it tests is broken.

The tests of the pipe, such as `TestRelayPath`, are in `sdk/direct`. The
[direct pipe spec](../../docs/direct/SPEC.md), section 8, lists them.

A test by hand on 2026-10-05 used a Mac on a home network and a virtual
machine in a data center, each behind a NAT, with no TURN server:

| Transfer | Result |
| --- | --- |
| 64 MiB, virtual machine to Mac | 50.0 s in all. The first attempt found no path, and the second attempt connected. |
| 64 MiB, Mac to virtual machine | 41.6 s. The first attempt connected. |
| 48 MiB, Mac to virtual machine, stopped with SIGTERM after 15 s | `get` said that the part file held 26525696 bytes. The second run got 23805952 bytes in 11.6 s. |

`sha256sum` on the two machines gave the same value for each file. Each
path was `srflx to srflx`.

A second test by hand on 2026-10-05 used `send` on the Mac, and a Claude
Code agent with the files adapter of gezibash/arc-harness on the virtual
machine:

| Sent with one `send` | Result |
| --- | --- |
| An image of 830 bytes | `send` printed the path and ended. The agent replied with the four colors of the image in the correct order. |
| A spoken sentence, WAV, 83244 bytes | `send` printed the path and ended. `sha256sum` gave the same value on the two machines. The agent replied that it cannot hear the file, because the machine has no tool that makes text from speech. |

A third test by hand on 2026-10-05 gave files with `put` to the transfer
app of a Claude Code agent on a virtual machine in a data center. The agent
served the app with `TRANSFER_PUT_MAX_MIB=50`, and the files adapter of
gezibash/arc-harness got each file with `get`:

| Sender | Result |
| --- | --- |
| A Mac on a home network, with `arc-transfer put` | 170874 bytes in 0.4 s, path `srflx to srflx`. A second `put` gave nothing: the app had the file. |
| An iPhone 14 Pro Max on a home network, with the iOS app of gezibash/arc-swift | A photo of 74540 bytes in 0.2 s, path `srflx to srflx`. `get` moved the file out of `received/`, and the agent answered about the photo. |

What no automated test covers:

- `put` between two machines, and `put` from a phone.
- Two machines on different networks.
- The second attempt after a first attempt with no path. The test of
  `-answerer-first` covers the first attempt with no wait, not the change of
  the order.
- A transfer that stops in the middle. The test of the part file starts from
  a part file that the test writes.
- The limit of 4 transfers at a time, and the error `busy`.
- `send` between two machines in an automated test.
- `send` to a receiver that has the file already. The files adapter of
  gezibash/arc-harness does not get a file two times, so `send` then waits
  until its time ends.
- A TURN server, a phone network, and a symmetric NAT.
