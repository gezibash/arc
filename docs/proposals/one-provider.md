Status: proposed

# One provider for each request: two devices of one citizen

No code implements this proposal. Each claim about ARC names a file or a
spec section at commit `6e9e1fc`. Each number has a tag:

- *measured*: a run on a real machine. The source is given.
- *estimated*: a judgement. No run supports it.

## 0. Bad news first

Today two providers with one identity both run each request (#447). In the
user test of #436, 20 of 20 calls and 10 of 10 sessions ran twice, and 153 of
223 during a restart (*measured*, Cato, ut-436.txt). The one-home lock of
#447 fixes this for one home: `TestOneHomeRunsEachRequestOnce` ran 5,562
calls through 4 overlapping restarts with none twice, and 1,022 of 6,493 ran
twice without the lock (*measured*, this Mac). Two devices of one citizen
that serve the same capability still both run each request. This proposal
fixes that case, and it is a change on the wire.

## 1. Why both run

A request names the provider by its identity only: the `p` tag of the
request (`core/call/call.go`, the tags of a request). Each `arc serve` of
that identity watches the wraps to that identity, so each gets and runs each
request. The relay is a plain Nostr relay: it gives each event to each watch
that matches.

## 2. Proposal: the announcement names the serving device

1. An announcement of a provider gets a `device` tag: the device key of the
   machine that serves it.
2. A caller copies that device into its request, as a new tag of the rumor.
3. A provider runs only the requests that name its own device, and a request
   with no device tag, as an older caller sends it.
4. The newest announcement wins (delivery spec, section 4.7), so the device
   that announced last serves, and the other devices skip each request.

What it costs:

- Two new tags on the wire: a delivery MUST, and a breaking change. An older
  provider ignores the tag, and runs each request, as today.
- An older caller sends no device tag, so at most once holds only for new
  callers.
- A device that announced last and then went away serves nothing until the
  other device announces again, at its next refresh (`catalog.Refresh`).
  The other device could announce again when it sees no answer; this needs
  a design of its own (*estimated*).

The relay stays a plain relay, so this works on any Nostr relay.

## 3. Rejected: a lease that the providers race for

Each provider would claim a lease, such as an addressable event per identity
and capability with a short expiry, and run requests only while it holds it.

Rejected, because the race window is where the duplicates happen: two
providers that start or restart together both see no lease, or both see an
old one end, and both run requests until one of them sees the other's lease.
A relay gives no compare-and-swap, so the lease cannot close that window
(*estimated*). It also costs a write to the relay at each renewal.

## 4. Open questions

- Should a provider that skips a request for another device tell the caller,
  or stay quiet? Quiet keeps the relay load as it is.
- Does a citizen want one device to serve, or should it be able to choose,
  such as a desktop that serves while it is on?
