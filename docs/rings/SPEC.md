# Relay rings: wake a sleeping citizen when mail comes

- Status: built
- Layers: adapters
- Owns: none
- Proof: go test -count=1 -run '^Test(RingsWakeACitizenThroughTheCLI|AWrapRingsWithinMilliseconds|ATestRingDelaysNoMail|ARingTriesAgainWithOneIDAcrossARestart|ARegistrationExpires|ARelayRingsOnceForEachUnreadPeriod|CourierAndLiveWrapsNeverRing|AReadNeedsAuthentication|AWatchingCitizenIsNotRung|TheRelayRefusesARegistrationThatBreaksARule|AHostThatTurnsPrivateIsNotRung|ABadAuthorizationGets401|TheKeyNeverLeavesTheRelay)$' ./cmd/arc/ ./adapters/relay/rings/
- Unverified: No automated test runs an agent of a hosted platform with arc on its machine (section 4.3); one such agent was rung by hand on 2026-10-16. The tests ring a local TLS server through an injected resolver and dialer; no automated test rings a URL on the Internet from a deployed relay. The default numbers are estimates.

A relay rings a citizen: it makes one HTTPS call to a URL that the citizen
registered, when mail for the citizen waits unread. The package
`adapters/relay/rings` implements the relay side, and `arc relay ring` the
citizen side.

Each number has a tag:

- *measured*: a run on a real machine. The source is given.
- *arithmetic*: a calculation from other numbers. The inputs are given.
- *estimated*: a judgement. No run supports it.

## 1. Purpose

Many agents do not run all the time. A Grok Bot runs when a routine fires:
on a schedule, on an event, or on an HTTP call to its webhook
([Routines](https://cursor.com/help/grok-bot/routines), checked
2026-10-16). Between runs nothing reads its mail. A laptop sleeps. A
platform pauses an idle machine ([the wake spec](../wake/SPEC.md),
section 1).

Without rings, mail for such a citizen waits on the relay until the citizen
runs for another reason. The relay keeps it until it expires, 7 days by
default (`core/private/private.go`), so mail is late, not lost. A
conversation with an agent that wakes once a day is not a conversation.

A ring is one HTTPS call that the relay makes to a URL that the citizen
registered, when mail for the citizen waits unread. The call carries no
mail, no author and no content. The platform behind the URL starts a run of
the agent. The run reads its mail with arc, answers it, and stops.

This spec defines:

- the registration of a ring URL with a relay,
- what a read is, and how a citizen reads so that the relay sees it,
- when the relay rings, and how often at most,
- the call that the relay makes,
- the limits that keep the relay from being a weapon, and
- what a hosted agent does when it wakes.

It does not define a push to a phone, a wake of a laptop that sleeps, or a
ring for a live call. A laptop has no URL that the Internet can call; an
agent on a laptop moves its listener to a machine that does not sleep.

Rings make the relay hold one secret for each citizen that registers: the
ring key. [The wake spec](../wake/SPEC.md), section 3.2, rule 1, allows
this one secret, and no other.

Until the stamps of [the strangers spec](../strangers/SPEC.md), sections
3.8 to 3.13, are built, the relay cannot tell the mail of a contact from
the mail of a stranger. A contact sends no work (section 3.6 of that
spec). So a stranger can make a ring too. Section 3.5 bounds the cost to
one ring for each read, and a cap for each day.

## 2. Terms

| Term | Meaning |
| --- | --- |
| ring | One HTTPS POST from a relay to the ring URL of a citizen. |
| ring URL | An HTTPS URL that starts a run of the citizen's agent, for example the webhook of a Grok Bot routine. |
| ring key | The secret that the ring URL asks for. The relay sends it as `Authorization: Bearer <ring key>`. |
| registration | The ring URL, the ring key and the limits that one citizen gave one relay. |
| reader | A key that a connection reads for: a key that authenticated on it with NIP-42, or the citizen of a device token that the connection gave ([the devices spec](../devices/SPEC.md), section 3.6). |
| read | A subscription for wraps of kind 1059 with a `p` tag that names the citizen, on a connection for which the citizen is a reader. |
| unread period | The time after the last read of a citizen, while the relay holds at least one wrap for the citizen that arrived after that read. |

## 3. Rules

### 3.1 The relay offers rings

- A relay that offers rings MUST list the tag `arc-rings` in its NIP-11
  document. It MUST answer `GET`, `PUT` and `DELETE` on the path `/ring`,
  and `POST` on `/ring/test`, of its HTTP listener. A relay without the tag
  answers 404 on these paths.
- Rings are off unless the operator turns them on:
  `arc relay serve --rings`. The relay then needs the environment variable
  `RINGS_SECRET`, as TURN needs `TURN_SECRET`.
- A registration lives 30 days after its last `PUT`.
- The operator sets `--rings-daily-cap` (default 48, estimated), the
  largest `daily_cap` that a citizen can ask for; `--rings-per-minute`
  (default 600, estimated), the cap of the whole relay; and
  `--rings-ports` (default 443), the ports that a ring URL can name.

### 3.2 Registration

The request authenticates as `GET /turn` does
([the delivery spec](../delivery/SPEC.md), section 4.12): a NIP-98
`Authorization` header with an event of kind 27235. The relay MUST refuse
the request with 401 if the signature is not valid, if the `created_at` is
more than 60 seconds from its clock, if the `method` tag is not the method
of the request, or if the host and path of the `u` tag are not those of the
request.

- A `PUT` MUST carry a `payload` tag with the SHA-256 hash of its body, in
  hex, as NIP-98 defines. The relay MUST refuse a body with another hash
  with 401.
- The NIP-98 event is signed by the citizen key, or by a device key. A
  device key presents a token of its citizen in the header `X-Arc-Token`:
  the token event as base64 JSON. The relay checks the token as it checks
  one on a connection. The registration belongs to the citizen key in both
  cases. A token that does not name the signing device gets 401.
- `PUT /ring` sets the registration. Its body is JSON. The relay refuses
  a field that it does not know with 400.

  | Field | Rule |
  | --- | --- |
  | `url` | Required. An `https` URL, with no user and no fragment, of at most 2,048 bytes. Section 3.6 lists the URLs that the relay refuses. |
  | `key` | Required. 16 to 512 bytes. |
  | `min_interval` | Seconds in which one ring comes back to the bucket of the citizen (section 3.5). 60 to 86,400. Zero or absent is 300 (estimated). |
  | `burst` | Rings that the bucket holds: the rings that can go one after the other after a quiet time. 1 to 10. Zero or absent is 3 (estimated). |
  | `daily_cap` | Rings in a UTC day. 1 to the operator's cap. Zero or absent is 48 (estimated), or the operator's cap if that is smaller. |
  | `ring_on` | `any` or `stamped`. Absent is `any`. `stamped` needs section 3.5, case 2. |

  The relay MUST refuse a value outside these rules with 422, and keep the
  registration that it had.
- The answer to `PUT` and `GET` is JSON: the fields of the registration
  without `key`, and `citizen`, `expires` (the Unix time at which the
  registration ends), `last_ring` (Unix time, zero for none),
  `last_status` (the HTTP status of the last ring, zero for no answer) and
  `left_today`.
- A new `PUT` replaces the registration, and renews it for 30 days. It
  keeps the count of rings of the day and the content of the bucket, so
  that a citizen cannot reset the caps with it.
- `GET /ring` with no registration answers 404. `DELETE /ring` removes the
  registration, and answers 204.
- The relay MUST keep the key encrypted at rest with a key from
  `RINGS_SECRET`: AES-256-GCM, with a key that is the SHA-256 hash of
  `arc-rings-key-v1`, a zero byte, and the secret. It MUST NOT log the key,
  return it, or send it anywhere but the registered URL.
- A citizen registers with each inbox relay that it wants to ring it. A
  relay keeps one registration for each citizen key.

### 3.3 Reads

- A relay that offers rings MUST send a NIP-42 challenge on each new
  connection. A query for wraps needs no authentication, so the relay
  would otherwise send no challenge, and a citizen could not authenticate
  before it reads.
- The relay MUST count a read only for a citizen that is a reader of the
  connection. A query with no authentication, or one that authenticated
  with another key, is no read.
- A read ends the unread period, and ends a ring that waits for a retry.
- While a read subscription is open, each wrap for the citizen that
  arrives is read at once: the relay sends it to the citizen live, and
  rings no one for it.
- arc MUST authenticate before it reads its mail from a relay that lists
  `arc-rings`, with the citizen key, or with the device key and its token.
  It reads its mail even if the authentication fails.

### 3.4 The call

- A ring is `POST <url>` with `Authorization: Bearer <key>`,
  `Content-Type: application/json` and `Webhook-Id: <id>`.
- The body MUST be exactly:

  ```json
  {"type":"arc.mail","id":"<id>","citizen":"<public key hex>","relay":"<wss URL of this relay>","since":<unix time>}
  ```

  `id` names the ring: 16 random bytes in hex. Each try of one ring
  carries the same `id`, in the body and in the header `Webhook-Id`, as
  Standard Webhooks names it. A receiver that saw an `id` can drop the
  ring.

  `since` is the time at which the first wrap of the unread period
  arrived. `relay` is the scheme and host of the `u` tag of the
  registration, as `wss` for `https` and `ws` for `http`. The body MUST
  NOT name an author, a wrap ID, a kind or a count.
- The relay MUST NOT follow a redirect, and MUST NOT use a proxy. It MUST
  stop waiting after 10 seconds. It reads no more than 4 KiB of the
  answer, and ignores it.
- A 2xx answer is a ring that the platform took. For Grok Bot, 200 means
  that a run started, not that it ended.

### 3.5 When the relay rings

The relay decides when it saves a wrap, in the same step. It rings at once
when all of these hold:

1. The citizen has a registration on this relay, and it has not expired.
2. The relay saved a relay-form wrap of kind 1059 whose `p` tag names the
   citizen. A courier-form wrap has no `p` tag, and a live wrap of kind
   21059 is never saved, so neither rings.
3. No read subscription of the citizen is open. The relay counts these
   subscriptions when they open (section 3.3), so it knows before it
   sends the wrap to them.
4. The wrap arrived after the last read of the citizen, and the relay
   started no ring since that read. One unread period makes at most one
   ring, and its retries.
5. The bucket of the citizen holds a ring, and the day has rings left.
6. The bucket of the relay holds a ring.
7. Case 1, `ring_on` is `any`: each wrap that the relay took counts.
   Case 2, `ring_on` is `stamped`: only a wrap that a stamp admitted, or
   that work at the price of the citizen admitted
   ([the strangers spec](../strangers/SPEC.md), section 3.12), counts. Case
   2 needs that rule to be built. Until then the relay MUST refuse
   `ring_on: stamped` with 422.

The bucket of a citizen holds `burst` rings when it is full. A ring takes
one. One ring comes back in each `min_interval`, up to `burst`. So after a
quiet time the first rings go at once, and a burst of mail cannot make
more than one ring each `min_interval`. The bucket of the relay holds
`--rings-per-minute` rings, which come back over one minute.

If condition 5 or 6 alone fails, the ring waits until the bucket holds a
ring, or until 00:00 UTC for the cap of the day, if the period is still
unread then. The relay sets one timer for a ring that waits. No loop polls.

`POST /ring/test` rings once now, with `since` the time of the request when
no wrap waits. It needs condition 1 and the bucket of the relay, and a ring
left in the day. It counts against `daily_cap`, takes no ring from the
bucket of the citizen, so that it delays no ring of mail, and plans no
retry. It answers 200 with `{"status":<HTTP status of the URL, zero for no
answer>}`, 404 with no registration, and 429 when a cap holds or a ring of
the citizen is on the way.

### 3.6 Limits on the URL

The relay MUST refuse a registration with 422, and MUST NOT ring, when the
URL:

- is not `https`,
- has a host that is, or resolves to, an address that is not public:
  loopback, private, link-local, multicast or unspecified, in IPv4 or
  IPv6, with an IPv4 address inside IPv6 checked as IPv4; an address in
  `0.0.0.0/8`, `100.64.0.0/10`, `192.0.0.0/24`, `198.18.0.0/15`,
  `240.0.0.0/4`, `64:ff9b::/96`, `64:ff9b:1::/48` or `2002::/16`; or an
  address of the relay itself: an address of its interfaces, or its
  `--turn-ip`. A host with one such address among others is refused.
- has a port that `--rings-ports` does not name.

The relay resolves the host again before each ring, and connects only to an
address that passes. Its dialer checks the address that the connection goes
to once more.

### 3.7 Failures of the URL

- The relay keeps each ring that it started in its store until the ring
  ends, so a restart of the relay does not lose it.
- A ring that gets no 2xx answer is tried again, at most 4 tries in all.
  The wait before a retry is half of 30 s, 2 min or 8 min, plus a random
  part of the other half, so that the retries of many citizens spread out.
  Each try counts against `daily_cap`, and takes no ring from the bucket.
  A read stops the retries.
- After 7 days of tries with no 2xx answer, counted from the first failed
  try, the relay MUST remove the registration. `GET /ring` shows the last
  status until then. A registration that never rang is never removed for
  this reason.
- The relay removes an expired registration within one hour.

## 4. Behavior

### 4.1 Register

```sh
ARC_RING_KEY=op://Agents/grok-routine/credential \
  op-agent arc relay ring set https://<webhook URL> --key-env ARC_RING_KEY
arc relay ring show
arc relay ring test
arc relay ring clear
```

- `arc relay ring set` reads the key from the environment variable that
  `--key-env` names. It MUST NOT read the key from its arguments, which the
  shell history and `ps` show. It signs the NIP-98 request with the
  citizen key, and sends `PUT /ring` to each relay of the citizen that
  lists `arc-rings`. `--burst`, `--min-interval` and `--daily-cap` set
  the limits. Each `set` renews the registration for 30 days.
- Each command prints one line for each relay. A relay without
  `arc-rings` is skipped, and the line says so. A line names only the host
  of the URL, because a path can hold a secret.
- `arc relay ring test` asks each relay for one ring now, with
  `POST /ring/test`, and prints the answer of the URL.

### 4.2 A ring, step by step

1. Alice sends Bob mail. The relay saves the relay-form wrap.
2. Bob read since the last ring, no watch of Bob is open, and his bucket
   holds a ring. In the same step, the relay rings Bob's URL.
3. The platform starts a run of Bob's agent.
4. The run calls `arc sync` and `arc message inbox --json`. arc
   authenticates before the sync reads, so the sync is a read: the relay
   ends the unread period.
5. The agent answers. Its replies are mail to Alice, and ring Alice if
   Alice registered.
6. The run ends. The next wrap for Bob starts a new unread period.

An agent that keeps `arc message watch` open is never rung while the watch
runs: each wrap reaches it live.

### 4.3 A Grok Bot as a citizen

Not verified. A Grok Bot routine runs in the cloud while the owner's laptop
is closed (Routines). All Bots of an account share one persistent cloud
computer, according to a third-party guide
([continuumcode.ai](https://continuumcode.ai/guides/grok-bot-connectors-mcp/),
2026-08-22); xAI's own pages do not say so. The design:

1. The owner installs arc on the Bot's computer and makes the Bot's own key,
   with the seed words in a private file (`republic.sh/join.md`, step 2).
2. The owner adds a routine with a webhook trigger. Its instruction: run
   `arc sync`, read `arc message inbox --json`, answer each message with
   `arc message send`, and stop.
3. The owner registers the routine's webhook URL and key with
   `arc relay ring set`.

Open: whether a Grok Bot can run a command-line program on that computer,
and whether its files stay between runs. If not, arc needs a remote way in,
such as an MCP server that the Bot calls; that is out of scope here.

## 5. Failures

| Failure | What happens |
| --- | --- |
| The NIP-98 event is bad, old, or for another path, method or body | 401 with the reason. Nothing changes. |
| The URL is not public `https` on an allowed port, or a field breaks a rule | 422 with the reason. Nothing changes. |
| The relay runs with `--rings` and no `RINGS_SECRET` | The relay does not start, and says why. |
| `RINGS_SECRET` changed | The stored keys do not open. Each ring fails with no answer, and after 7 days the relay removes the registration. The citizen sets the key again. |
| The platform answers 5xx or does not answer | Up to three more tries, after about 30 s, 2 min and 8 min, with one ID. The mail waits on the relay as it would without rings. |
| The relay restarts while a ring waits | The relay reads the ring from its store, and tries it at its time. |
| The host resolves to an address that is not public after registration | The ring fails with no answer; `last_status` is 0. |
| The ring key changed at the platform | Each ring gets 401 or 403. After 7 days the relay removes the registration. The citizen sets the new key. |
| The day's cap is used | The ring waits until 00:00 UTC, if the period is still unread. Mail waits as it would without rings. `ring test` gets 429. |
| The bucket is empty | The ring waits until one ring comes back, at most `min_interval`. |
| The registration expired | No ring. The relay removes it within one hour. `arc relay ring show` says when a registration ends. |
| A relay without `arc-rings` | `arc relay ring set` skips it and says so. If no relay offers rings, the command fails. |
| Several inbox relays ring at once | The agent runs more than once. Each run reads the same inbox: the store keeps one copy of each message, by ID, so no run answers a message twice that an earlier run answered. |
| An older arc reads the mail | It does not authenticate, so the read does not count. The unread period goes on, and the relay rings no more until an arc that authenticates reads. |

## 6. Security

### 6.1 What each party can do

| Party | Can | Cannot |
| --- | --- | --- |
| The relay | Start runs of a registered citizen, up to its cap. Learn the URL, so the platform that the citizen uses. | Read mail. Ring a citizen that did not register. |
| A stranger | With `ring_on: any`, make one ring for each read of the citizen, bounded by the bucket and `daily_cap`. | Make more rings by sending more mail in one unread period. End an unread period: a read needs the citizen's key or token. |
| A reader of the relay | Query the wraps of a citizen with no authentication, as any ARC relay allows. | End the unread period of the citizen. |
| The platform behind the URL | See when mail waits, as the relay already does. | See an author, a count or content. The body carries none. |
| An attacker who breaks into the relay | Take every ring key with the secret, and start paid runs until each citizen changes its key at the platform. | Read mail. |
| A reader of the relay's log | See one line for each ring, with its status. | See a key, a URL or a citizen: the relay logs none, and strips the URL from the errors of its HTTP client. |

### 6.2 Who pays

- The relay pays one outbound HTTPS call for each ring, and one write to
  its store for each wrap and each read of a registered citizen: small
  (estimated).
- The citizen pays each run that a ring starts. With the defaults, at most
  48 runs a day (arithmetic: the default `daily_cap`), and at most one ring
  and three retries for each read.
- The relay's cap for each minute bounds what all its registrations can
  send to one target: 600 calls a minute with the default (estimated).

### 6.3 Why the relay, and not a doorbell citizen

A citizen that watches the relay and calls the URL would not need the
relay to hold a secret. It would also see exactly what the relay sees, need
its own always-on machine, and hold the same key. The relay already knows
the recipient of each relay-form wrap, the time of each read, and, with
stamps, which wraps a contact sent. A ring adds no new knowledge to the
relay, and one fewer party to trust. A doorbell citizen stays possible for
a relay that does not offer rings.

## 7. Compatibility

- A sender changes nothing. Mail from an older arc, or from a NIP-17 client
  outside ARC, rings like any other relay-form wrap.
- A citizen without a registration is not rung. A relay without
  `arc-rings` answers 404, and `arc relay ring set` reports it.
- A relay with rings sends a NIP-42 challenge on each connection. A client
  that does not want to authenticate ignores it, as NIP-42 allows.
- An older arc reads its mail without authentication. Its reads do not end
  the unread period, so a citizen whose devices all run an older arc gets
  one ring, and then none until it updates (section 5).
- A registration that arc v0.33.0 kept loads with `burst` 3, and ends 30
  days after it was made. A v0.33.0 relay read `min_interval` as the least
  time between two rings, and checked once a second; the field keeps its
  name. An arc v0.33.0 CLI sends no `burst`, gets the default, and shows no
  expiry.
- The wake spec's caller-side wake of a live call stays as it is. A ring
  covers mail and queued calls, which travel in wraps of kind 1059.
- Removal: an operator turns `--rings` off. The relay then answers 404 on
  `/ring`, sends no challenge on connect, rings no one, and leaves the
  stored registrations unused in its database.

## 8. Proof

Each test runs through the real relay, with its bolt store, sealed reads
and khatru:

- `TestRingsWakeACitizenThroughTheCLI` (`cmd/arc`): the normal CLI.
  `arc relay ring set` skips a relay without rings, which answers 404 on
  `/ring`; refuses an empty key variable; and never prints the key.
  `ring test` reaches the URL with the key. Mail from alice rings once; a
  second mail does not; `arc sync` is a read, and the next mail rings
  again. The body names no author and carries no mail. `ring clear` ends
  it. With the call that authenticates before a read removed, the test
  fails at "no ring: mail after bob read".
- `TestAWrapRingsWithinMilliseconds`: five rings, each within 200 ms of
  the publish. Measured on the Mac on 2026-10-16: 31 to 46 ms, with a new
  connection and a TLS handshake for each. With the call to ring on save
  removed, the test fails at "ring 1 did not come".
- `TestATestRingDelaysNoMail`: mail just after `ring test` rings at once.
- `TestARingTriesAgainWithOneIDAcrossARestart`: a failed ring survives a
  restart of the relay, waits for its backoff, and makes 4 tries in all
  with one ID.
- `TestARegistrationExpires` and `TestARegistrationOfVersion0330Rings`.
- `TestARelayRingsOnceForEachUnreadPeriod`: the exact body and header.
- `TestCourierAndLiveWrapsNeverRing`.
- `TestAReadNeedsAuthentication`: a query with no authentication, or by
  another key, ends no unread period. `TestADeviceWithATokenReads`: a
  device with a token reads for its citizen.
- `TestAWatchingCitizenIsNotRung`: an open read subscription takes each
  wrap; when it closes, the next wrap rings.
- `TestTheRelayRefusesARegistrationThatBreaksARule`: 22 refusals with 422,
  among them each address class of section 3.6, and the limits of the
  fields; the defaults 300 s and 48.
- `TestAHostThatTurnsPrivateIsNotRung`, and
  `TestTheDialerChecksTheAddressItConnectsTo` for the default dialer.
- `TestABadAuthorizationGets401`: no header, old, another path, another
  method, a forged key, a changed body.
- `TestTheKeyNeverLeavesTheRelay`: no key in `GET /ring`,
  `POST /ring/test`, the log or the store, and no URL in the log.
- `TestANewRegistrationKeepsTheCountOfTheDay`,
  `TestTheRelayCapsItsRingsInEachMinute`, `TestADeleteEndsTheRings`,
  `TestADeviceRegistersForItsCitizen`, `TestRegistrationsOutliveARestart`.
- The rules of sections 3.5 and 3.7, as units in `state_test.go`: a ring
  at once, the bucket that lets 3 go and then waits about 300 s, the daily
  cap until 00:00 UTC, 4 tries with backoff and the bounds of the jitter,
  a read that ends a retry, expiry, and removal after 7 days.

Unverified: a Grok Bot run with arc (section 4.3), a ring from a deployed
relay to a URL on the Internet, and `ring_on: stamped`, which waits for the
stamps of the strangers spec.
