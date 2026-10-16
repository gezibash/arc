# Event kinds

This is the registry of every Nostr event kind that ARC reads, writes or
reserves. `go test ./internal/specs` checks it against the code: each kind
constant in `core/`, `runtime/` and `sdk/`, each `"kind"` in a manifest under
`apps/`, and the reserved kinds in `runtime/iface/consent.go` must appear
here, and each row here must exist in one of those places.

A new kind needs a row here, a reason for its class, and a spec section.
The class follows NIP-01: kinds 0 and 3, and 10000 to 19999, are replaceable;
20000 to 29999 are ephemeral; 30000 to 39999 are addressable; each other kind
is regular. "272" spells ARC on a
phone keypad; the ARC kinds end in 272 to 276, see the delivery spec,
section 3.7.

| Kind | Class | Defined by | Use | Source | Spec |
| --- | --- | --- | --- | --- | --- |
| 0 | replaceable | NIP-01 | Profile. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 3 | replaceable | NIP-02 | Follows. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 5 | regular | NIP-09 | Deletion. Core makes it for `delete`, through `nostr.KindDeletion`. A device also sends one to a relay for the old drafts that a storage citizen holds, with the tag `["arc", "moved"]`. | reserved | [interface, section 6](interface/SPEC.md), [storage, section 4.2](storage/SPEC.md) |
| 13 | regular | NIP-59 | A seal inside a gift wrap. | code | [delivery, section 3.4](delivery/SPEC.md) |
| 14 | regular | NIP-17 | A direct message, as a rumor. | code | [delivery, section 4.2](delivery/SPEC.md) |
| 62 | regular | NIP-62 | Request to vanish. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 1059 | regular | NIP-59 | A gift wrap that a relay keeps. | code | [delivery, section 3.4](delivery/SPEC.md) |
| 1063 | regular | NIP-94 | File metadata: a sealed file. | manifest | [files app](../apps/files/README.md) |
| 1234 | regular | NIP-37 | A checkpoint of a draft: one revision of a sealed event. A device key signs it, and a `p` tag names the citizen. It stays on the device and on storage citizens. An ARC relay refuses it. | code | [interface, section 3.9.1](interface/SPEC.md), [storage, section 7](storage/SPEC.md) |
| 3272 | regular | ARC | A call request, inside a gift wrap. | code | [delivery, section 4.10](delivery/SPEC.md) |
| 3273 | regular | ARC | A call reply, inside a gift wrap. | code | [delivery, section 4.10](delivery/SPEC.md) |
| 3274 | regular | ARC | An acknowledgement of mail, inside a gift wrap. | code | [delivery, section 4.2](delivery/SPEC.md) |
| 3275 | regular | ARC | One continuation part of sealed content longer than 32 KiB. A device key signs it, and a `p` tag names the citizen. It stays on the device and on storage citizens. An ARC relay refuses it. | code | [interface, section 3.9.2](interface/SPEC.md), [storage, section 7](storage/SPEC.md) |
| 3276 | regular | ARC | Core session frames, inside live wraps. | code | [sessions](sessions/SPEC.md) |
| 9734 | regular | NIP-57 | Zap request. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 9735 | regular | NIP-57 | Zap receipt. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 10002 | replaceable | NIP-65 | The relay list of a citizen. | code | [delivery, section 3.8](delivery/SPEC.md) |
| 10013 | replaceable | NIP-37 | Retired. The relay list for drafts. ARC does not make it, because no relay holds drafts. An ARC relay refuses it, and a device deletes the old ones. No manifest can name it. | reserved | [storage, section 7](storage/SPEC.md) |
| 10050 | replaceable | NIP-17 | The relay list for direct messages. | code | [delivery, section 3.8](delivery/SPEC.md) |
| 10272 | replaceable | ARC | Reserved, and not used. | reserved | [delivery, section 3.7](delivery/SPEC.md) |
| 10273 | replaceable | ARC | The storage list of a citizen: its storage citizens, sealed to the citizen key. Replaceable, because each citizen has one list, and each change replaces it in place. A relay serves it only to its author. | code, reserved | [storage, section 3.8](storage/SPEC.md) |
| 10274 | replaceable | ARC | The device list of a citizen, sealed to the citizen key. A relay serves it only to its author. | code, reserved | [devices, section 3.2](devices/SPEC.md) |
| 10275 | replaceable | ARC | The data roots of a citizen, wrapped to the citizen key and to each device key, sealed to the citizen key. A relay serves it only to its author. | code, reserved | [devices, section 3.5](devices/SPEC.md) |
| 13194 | replaceable | NIP-47 | Wallet connect. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 20274 | ephemeral | ARC | The token of a device: the citizen key lets one device key read the sealed data of the citizen on a relay, until an end at most 7 days ahead. Ephemeral, because a relay checks it on one connection and never keeps it. | code, reserved | [delivery, section 4.13](delivery/SPEC.md), [devices, section 3.6](devices/SPEC.md) |
| 21059 | ephemeral | ARC | A live gift wrap. A relay never keeps it. | code | [delivery, section 3.4](delivery/SPEC.md) |
| 22242 | ephemeral | NIP-42 | Relay authentication. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 23194 | ephemeral | NIP-47 | Wallet connect request. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 23195 | ephemeral | NIP-47 | Wallet connect response. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 24133 | ephemeral | NIP-46 | Remote signing. No manifest can name it. | reserved | [interface, section 6](interface/SPEC.md) |
| 27235 | ephemeral | NIP-98 | HTTP authentication. `arc relay turn` signs it to get a TURN credential from a relay, and `arc relay ring` to register a ring URL. No manifest can name it. | code, reserved | [interface, section 6](interface/SPEC.md), [delivery, section 4.12](delivery/SPEC.md), [rings, section 3.2](rings/SPEC.md) |
| 30023 | addressable | NIP-23 | A long-form article: a journal page or index, inside a draft. | manifest | [journal app](../apps/journal/README.md) |
| 30078 | addressable | NIP-78 | Application data: the keyed root, a journal KPI, and one contact of the contact list, each inside a draft. | code, manifest | [interface, section 3.7.1](interface/SPEC.md), [strangers, section 3.1](strangers/SPEC.md), and the [journal app](../apps/journal/README.md) |
| 30272 | addressable | ARC | A capability announcement. | code | [delivery, section 4.7](delivery/SPEC.md) |
| 31234 | addressable | NIP-37 | A draft: sealed data of the citizen. A device key signs it, a `p` tag names the citizen, and a `gen` tag names the data root ([interface, section 3.9.4](interface/SPEC.md)). It stays on the device and on storage citizens. An ARC relay refuses it, except the draft of the keyed root. | code | [interface, section 3.9](interface/SPEC.md), [storage, section 7](storage/SPEC.md) |

The `Source` column says where the test finds the kind: `code` is a literal
constant in Go, `manifest` is a `"kind"` field under `apps/`, and `reserved`
is the list in `runtime/iface/consent.go`. Kind 5 appears in code only as the
library constant `nostr.KindDeletion`, which the test does not scan.

Two ranges are rules, not rows: kinds 9000 to 9020 moderate a NIP-29 group,
and kinds 39000 to 39009 are group state that only a relay signs. See the
interface spec, section 6.
