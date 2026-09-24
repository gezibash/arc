# Linux Bluetooth radio probe (PR 3)

This stage adds a small hardware diagnostic, `arc-ble-probe`. It advertises an
ARC test service, discovers other devices running it, and optionally connects
to one chosen device to send 20 random bytes and read the same bytes back.
Both devices keep their peripheral service active while acting as a central.

This is not yet an ARC event transport. It does not send private messages,
call capabilities, forward traffic, pair devices, or authenticate ARC identities.
The test service is unencrypted and uses its own UUIDs so it cannot be confused
with a future secure service. Only randomly generated test bytes are sent.

## Requirements

Use two Linux machines with BLE adapters supporting central and peripheral
roles together, BlueZ running, and permission to access BlueZ on the system
D-Bus. Each adapter must expose `GattManager1` and `LEAdvertisingManager1`.

Check your adapter with `bluetoothctl show`. If needed, enable Bluetooth using
your system settings or `bluetoothctl power on`, and check airplane mode/rfkill.
The probe does not change power or system permissions for you. A D-Bus access
error requires the machine's administrator to configure access; do not disable
system bus security or run the whole application as root as a default fix.

macOS support remains PR 8. On other operating systems the command reports that
this probe requires Linux. No ARC identity, relay, Wi-Fi or internet connection
is needed for the radio test after the binary is built.

## Build

In the ARC repository on Linux:

```sh
mise exec -- go build -o bin/arc-ble-probe ./cmd/arc-ble-probe
```

To build on a Mac for a Linux x86_64 machine:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 mise exec -- go build -o /tmp/arc-ble-probe-linux ./cmd/arc-ble-probe
```

Use `GOARCH=arm64` for a Linux ARM64 machine. Copy the matching binary to the
Linux test machine. The installed `arc` binary does not gain Bluetooth commands
from this stage; the probe is built separately.

## Test two devices

On machine B:

```sh
./bin/arc-ble-probe --adapter hci0 --timeout 5m
```

It prints its adapter address and starts advertising and scanning. On A, start
with discovery:

```sh
./bin/arc-ble-probe --adapter hci0 --timeout 15s
```

A discovered peer appears as `peer AA:BB:CC:DD:EE:FF`. Discovery may include BlueZ
cached devices; an address alone is not proof of a live connection. Use B's
address for the actual check, while B's probe is still running:

```sh
./bin/arc-ble-probe --peer AA:BB:CC:DD:EE:FF --timeout 30s
```

Success prints `PASS: 20 bytes echoed by AA:BB:CC:DD:EE:FF` and exits. That proves
a write/read round trip, not identity authentication. The command keeps its own
advertisement active during the check, stops its discovery session, disconnects
the connection it attempted, and unregisters its advertisement and service on
exit. Ctrl-C also triggers cleanup. Cleanup errors are reported. An already
connected peer is refused so the probe does not take over another application's
connection; use a test peer with no existing connection.

Repeat with A listening and B making the connection. Disconnect or power off the
peer to check that the caller fails within the requested timeout (plus bounded
cleanup calls). Timeout applies to the complete scan/connect/resolve/exchange
operation. Each cleanup D-Bus call has a separate three-second limit.

If the adapter supports several radios, select the intended one with `--adapter`.
Do not run two probe processes on the same adapter for this test. No MAC address
is treated as an ARC identity, and a device changing its address needs to be
rediscovered.

## Test profile and limits

- Service UUID: `8f71c100-5b9a-4cf0-a8d1-3e290da1b301`.
- Read/write echo characteristic: `8f71c101-5b9a-4cf0-a8d1-3e290da1b301`.
- Writes use acknowledged GATT requests; reads return the latest test bytes for
  that same BlueZ device. No notifications or continuous duplex stream yet.
- Exactly 20 bytes per write, fitting the default ATT MTU of 23. This does not
  claim a larger negotiated payload size or exercise event fragmentation.
- At most 16 peers' samples (320 payload bytes) are held. Samples expire after
  30 seconds and are removed on the next read/write or when the probe exits.
- Nonzero offsets and prepared writes are refused. Samples are copied, not
  shared with caller buffers. Concurrent BlueZ callbacks are protected by a lock.
- D-Bus read/write callbacks accept only BlueZ's unique bus sender. This protects
  local callback access; it does not authenticate the remote Bluetooth device.

The direct BlueZ backend uses `github.com/godbus/dbus/v5`. The TinyGo Linux
backend inspected for this stage reports server writes with connection ID zero;
ARC needs the BlueZ `device` option to keep responses separate per peer. This
uses the fallback allowed by the delivery spec, without maintaining a library
fork. No upstream patch is part of this PR.

References: [BlueZ GATT characteristic API](https://github.com/bluez/bluez/blob/master/doc/org.bluez.GattCharacteristic.rst),
[advertisement API](https://github.com/bluez/bluez/blob/master/doc/org.bluez.LEAdvertisement.rst),
and [TinyGo Linux server](https://github.com/tinygo-org/bluetooth/blob/release/gatts_linux.go).

## Automated checks and remaining proof

```sh
mise exec -- go test -race ./delivery/transport/ble ./cmd/arc-ble-probe
```

Fake-radio tests cover the client round trip, corrupted replies, connection and
write failures, cancellation, cleanup, rejecting existing connections and
selecting a characteristic belonging to the right device. Server tests cover
per-peer isolation, sample expiry, bounded storage, malformed writes, buffer
ownership, caller checks and simultaneous read/write callbacks.

These tests do not prove controller or BlueZ interoperability. Before treating
PR 3 as hardware-verified, record Linux/BlueZ versions, adapter models, both
round-trip results, discovery while advertising, cancellation and restart
results here or in the PR. Hardware validation is pending; no Linux radio was
available in the implementation environment. Full phase 5, including the
three-node mesh proof, remains incomplete.

PR 4 adds secure direct sessions. Later integration will carry frames from
`delivery/frame`, supply negotiated usable payload sizes, schedule fragment
expiry and verify completed events through ARC's store.
