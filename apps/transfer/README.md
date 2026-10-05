# Transfer app

The transfer app gives a file to a citizen on a direct connection. ARC
carries the request and the answer. The bytes do not go through a relay, so
a file has no size limit.

The [manifest](manifest.json) declares the service. `server/` answers the
requests, `client/` gets a file, and `direct/` holds what the two ends share.
`cmd/arc-transfer` is the program: the service, and the two commands `offer`
and `get`.

| Part | Status |
| --- | --- |
| `offer`, `get` and the service | Built. Tested with the built `arc`, a local relay and three citizens, and by hand between two machines behind NATs. |
| A relay server (TURN) for two machines that find no direct path | Not built. |
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
- A STUN server that each machine can reach. The default is
  `stun:stun.l.google.com:19302`. The server sees the public address of each
  machine. The two machines see the public address of each other. A relay
  does not show these addresses.
- The two machines online at the same time.

## Use it

Build with `mise run build`, and use `bin/arc` and `bin/arc-transfer`.

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

Install the app of the sender one time:

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
means that the path goes through a NAT.

If a transfer stops, run the same command again. `get` reads the part file,
and gets only the rest.

## Settings

The service reads its settings from the environment. `arc serve` gives its
environment to the program.

| Variable | Value |
| --- | --- |
| `TRANSFER_STATE` | The directory of the offers. The default is `~/.local/state/arc-transfer`. `offer` and the service must use the same directory. |
| `TRANSFER_STUN` | The STUN server, or `none`. `get` reads it too. |
| `TRANSFER_MIN_WINDOW_KIB` | The send window stays at or above this size after a loss. This makes a transfer faster on a path with losses. It is not fair to other traffic. |
| `TRANSFER_LOOPBACK` | If set, an end also uses the loopback address. The tests set it, because their two ends are on one machine. `get` reads it too. |

The commands take these flags:

| Command | Flag | Value |
| --- | --- | --- |
| `offer`, `get` | `-arc <program>` | The arc program. The default is `arc` on `PATH`. |
| `offer`, `get` | `-home <directory>` | The arc home. The default is the home that arc picks. |
| `offer` | `-state <directory>` | The directory of the offers. The default is `TRANSFER_STATE`. |
| `offer` | `-to <key>` | A public key that can get the file. |
| `get` | `-o <file>` | The file to write. The default is the name in the link, in the current directory. |
| `get` | `-stun <url>` | The STUN server, or `none`. The default is `TRANSFER_STUN`. |
| `get` | `-hold` | Ask the sender to wait in the first attempt. See "The order of the first packets". |

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
- Each description holds all the addresses of its end. The app does not
  send addresses later.

The receiver opens one data channel. The sender writes the bytes from
`offset` in messages of 16 KiB, and then the text `end`. The receiver
answers with the text `ok`.

A refused request has one of these errors:

| Error | Cause |
| --- | --- |
| `invalid_request` | The body is not a request of version 1. |
| `unknown_offer` | The sender has no offer with this SHA-256 for this caller. |
| `file_changed` | The size or the modification time of the file changed after the offer. |
| `busy` | The sender runs 4 transfers. |
| `transfer_failed` | The sender could not open its end of the connection. |

## Rules of the sender

- A caller gets a file only if an offer for it is in the state directory. If
  the offer names keys, the caller must have one of them. A caller with
  another key gets `unknown_offer`, the same answer as for no offer.
- The sender refuses a file that changed after the offer. Record the file
  again.
- To limit the callers of the app, set `allow` in the `Arcfile`.
- An offer stays until you delete its file `offers/<sha256>.json` in the
  state directory.

## Rules of the receiver

- `get` does not replace a file. If the output file is there, it stops.
- The name in a link comes from another citizen. `get` uses only its last
  part, with no directory.
- If the bytes do not have the SHA-256 of the link, `get` removes the part
  file, and writes no file.
- If the sender writes more bytes than the size in the link, `get` does not
  write them, removes the part file, and writes no file.

## The order of the first packets

On some routers, a direct path opens only if the machine behind that router
sends the first packet. If a packet of the other machine arrives first, no
packet crosses in each direction.

`get` therefore makes 2 attempts. In the first attempt, the sender sends
first. If the data channel does not open in 12 seconds, `get` asks the
sender to wait 1 second, so that the receiver sends first. With `-hold`,
`get` uses the other sequence.

If each machine is behind such a router, no order connects. `get` then
stops with `no direct path`.

## Why the settings are as they are

- **Network interfaces.** An end does not use the interfaces of virtual
  machines and tunnels (`bridge`, `vmenet`, `utun`, `docker` and others, see
  `direct/direct.go`). A UDP write on such a bridge blocked with no end, and
  pion then stopped all ICE work.
- **UDP on IPv4 only.** STUN on IPv6 with no route added 5 seconds to each
  setup.
- **ICE timing.** pion stops after 8 pings for each pair of addresses, about
  1.6 seconds, and reports a failure after 30 seconds. An end pings for
  6 seconds, and reports a failure after 8 seconds.
- **Congestion control.** The SCTP of pion slows down much after a loss on
  a path of 50 ms. `TRANSFER_MIN_WINDOW_KIB` is a workaround, not a
  solution.

## Tests

```sh
go test ./apps/transfer/...
```

The tests build `arc` and `arc-transfer`, and run them as a person does: a
relay on this machine, three citizens, and `arc serve`. Each test fails when
the behavior that it tests is broken.

A test by hand on 2026-10-05 used a Mac on a home network and a virtual
machine in a data center, each behind a NAT, with no TURN server:

| Transfer | Result |
| --- | --- |
| 64 MiB, virtual machine to Mac | 50.0 s in all. The first attempt found no path, and the second attempt connected. |
| 64 MiB, Mac to virtual machine | 41.6 s. The first attempt connected. |
| 48 MiB, Mac to virtual machine, stopped with SIGTERM after 15 s | `get` said that the part file held 26525696 bytes. The second run got 23805952 bytes in 11.6 s. |

`sha256sum` on the two machines gave the same value for each file. Each
path was `srflx to srflx`.

What no automated test covers:

- Two machines on different networks.
- The second attempt after a first attempt with no path. The test of `-hold`
  covers the wait of the sender, not the change of the order.
- A transfer that stops in the middle. The test of the part file starts from
  a part file that the test writes.
- The limit of 4 transfers at a time, and the error `busy`.
- A TURN server, a phone network, and a symmetric NAT.
