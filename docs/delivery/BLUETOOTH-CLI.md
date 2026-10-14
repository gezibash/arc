# Test ARC commands over Bluetooth on Linux

Status: **Integrated, verification pending.** These are acceptance
instructions. The automated CLI test replaces the radio only. Two Raspberry Pis
passed these steps, with the conditions and open items in
[Recorded results](#recorded-results). The earlier Pi/iPhone probe test tested
a different profile.

## What you need

- Two Linux machines with powered BLE adapters and BlueZ. A Pi 3 B+ with a
  64-bit Debian system is a candidate; support still needs this test. The other
  device can be another Pi or a Linux laptop.
- The same Stage 5 build on both machines. macOS, iPhone and Android cannot act
  as this ARC endpoint yet. A Linux container on a Mac does not supply BlueZ
  access to the Mac's Bluetooth controller.
- Permission to use the system D-Bus Bluetooth APIs. Check `bluetoothctl show`
  and `bluetoothctl power on`. If D-Bus refuses registration, use the distribution's
  Bluetooth policy or a dedicated test account with the required permission.
- A local ARC signing key on each device. Remote signing is refused in this mode.
- No Bluetooth pairing between the two devices. ARC does not use pairing. If a
  desktop asks to pair, decline. A bonded device can be connected by BlueZ for
  another profile, such as MIDI, and ARC then refuses the busy peer. To remove
  a pairing, run `bluetoothctl remove <other device's address>` on each device.

Build on Linux from the repository root (Go version is in `mise.toml`):

```sh
go build -o arc ./cmd/arc
go build -o arc-echo ./adapters/provider/host/testdata/echo
```

`arc-echo` is a small test service. It returns the request text and does not
execute it. Normal ARC commands handle discovery, installation and calls.
Copy `cmd/arc/testdata/ble-echo.json` with the binaries when using another directory.
For a Pi cross-build, prefix each build with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`.
A cross-build proves compilation, not radio support.

## Choose identities and start the receiving device

On each device, make a separate test home:

```sh
./arc --home ./ble-home keys gen
./arc --home ./ble-home whoami
```

Keep the recovery words private. Exchange only the 64-character public key on
`whoami`'s second line, through a channel you trust. The receiver is B; the caller
is A. Set `A_KEY` on B and `B_KEY` on A to those public keys.

On B, find its controller address with `bluetoothctl show`. On A, set `B_ADDRESS`
to that address. The Bluetooth address locates B; its ARC key authenticates B.

Start B from the repository root:

```sh
./arc --home ./ble-home --bluetooth-peer "$A_KEY" --bluetooth-listen \
  serve "exec://$(pwd)/arc-echo?manifest=$(pwd)/cmd/arc/testdata/ble-echo.json"
```

Leave it running. It advertises while waiting. The `serves` line appears after
A connects and the live call listener is ready. It can accept later connections
from A without restarting the service.

## Discover, install and call from A

Define a short shell helper on A so every command selects the same direct path:

```sh
arc_ble() {
  ./arc --home ./ble-home --bluetooth-peer "$B_KEY" \
    --bluetooth-address "$B_ADDRESS" "$@"
}
arc_ble discover
arc_ble install "$B_KEY" echo --yes
arc_ble call "echo+arc://$B_KEY/" hello
arc_ble call "echo+arc://$B_KEY/" again
arc_ble echo say installed
```

Expect an `echo` announcement, installation confirmation, then `ECHO / hello`,
`ECHO / again` and `ECHO / installed`. The last line comes from the installed
command `say`, which the manifest declares. Each CLI invocation opens a new authenticated connection.
Calls within one command can share that connection. `--bluetooth-adapter hci1`
selects another controller if needed. Installed-command configuration flags go
before the installed app name.

Bluetooth selection excludes configured and discovered internet relays, indexers,
inbox relay lookup and wake hooks. It does not publish empty relay lists or alter
saved relay settings. Disabling Wi-Fi and unplugging Ethernet after copying the
files gives an additional hardware check. Hosted applications may themselves use
networks; this mode controls ARC event delivery, not a process sandbox.

## Send private mail and check the receipt

While B still serves:

```sh
arc_ble message send "$B_KEY" "hello over Bluetooth"
arc_ble sync
arc_ble message outbox
```

A transport acknowledgement is not a mail receipt. The outbox should eventually
show `delivered` after B processes the message and A syncs its acknowledgement.
Another explicit sync may be needed if B had not processed it yet.

Stop B's service with Ctrl-C, then inspect its stored inbox:

```sh
./arc --home ./ble-home message inbox
```

Expect one copy of the message. You can instead run B as a message receiver with
`--bluetooth-peer "$A_KEY" --bluetooth-listen message watch`. That command accepts
mail but does not provide the echo service. Do not open two commands against the
same home at once.

## Failures to verify and record

- Repeat sync: the inbox still shows one message.
- Stop B during a call: A must report an error. An interrupted call's result can
  be uncertain; ARC must not replay it automatically.
- Restart B: an explicit new command on A can connect again. Old sessions end.
- Cancel a running command with Ctrl-C: its connection and registrations close.
- Give the wrong peer key on A: A fails within a few seconds and asks to check
  the key. B refuses that connection and keeps waiting for the right peer, so
  a correct command afterwards works without a restart of B.
- Turn B's adapter off: A fails within its connection/operation deadline, with
  no internet relay fallback. A missing peer has a 30-second setup limit. Turn
  it on again: B serves again within a few seconds, without a restart. If A
  connected in the moment B switched off, BlueZ on A can keep a connection that
  no command owns, and the next command reports the peer as busy. Clear it with
  `bluetoothctl disconnect <B's address>` on A. This is an open problem in the
  delivery spec, section 8.3.

Record both device models, OS/kernel, BlueZ version, build commit, commands,
results and failures in the PR.

## Recorded results

Run on 2026-10-14 with build `759de36`.

| Device | Board | OS | Kernel | BlueZ |
| --- | --- | --- | --- | --- |
| A, caller | Raspberry Pi 5 Model B Rev 1.1 | Debian 13 | 6.18.50+rpt-rpi-2712 | 5.82 |
| B, receiver | Raspberry Pi 3 Model B Plus Rev 1.3 | Debian 13 | 6.12.62+rpt-rpi-v8 | 5.82 |

The devices were not paired. Bluetooth MIDI was off on both: `bluetoothd
--noplugin=midi`, and `monitor.bluez-midi = disabled` in WirePlumber. With
MIDI on, every connection asked to pair and setup hit its limit; see the open
problems in the delivery spec, section 8.3.

| Step | Result |
| --- | --- |
| `discover` | Passed, about 7 s. |
| Four calls in a row, one through the installed command `say` | Passed. Round trip 1.3 to 2.3 s; 7 to 15 s for each command. |
| `message send`, two `sync`, `message outbox` | Passed. The outbox shows `delivered`. B's inbox holds one copy. |
| Restart B, then call | Passed. |
| Wrong peer key on A | Passed. A failed in about 9 s. B kept serving, and the correct key worked next. |
| Stop B during a call | Passed. A reported an unknown outcome, and did not repeat the call. |
| Ctrl-C on A | Passed. A stopped in about 3 s and left no scan or connection. |
| B's adapter off, then on | A failed at its limit, with no network connection. B served again without a restart. Open item: a connection that A started as B went off stayed in BlueZ, and the next command found B busy until `bluetoothctl disconnect`. |

Not verified: a run with Wi-Fi and Ethernet physically off. ARC was in
Bluetooth-only mode, and no ARC process had a network connection, but the
Pis stayed on Wi-Fi for remote control.

This stage supports one trusted peer at a time. It has no mesh, room relay,
automatic fallback or bulk/self-data sync. `sync --dir`, repair, TURN and import
options cannot be combined with Bluetooth mode. Only this citizen's service
announcements and sealed mail addressed to the peer are available to remote
queries. Query snapshots are limited to 64 events; large histories need narrower
filters and may exceed the scope of these initial CLI tests.

See [the adapter guide](BLUETOOTH-DIRECT.md) for encryption and size limits.
