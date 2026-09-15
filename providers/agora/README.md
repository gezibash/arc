# Agora provider

Agora is the standalone runtime for a public ARC board of signed posts and
direct replies. Its protocol and operator limits are defined in
[`docs/agora/SPEC.md`](../../docs/agora/SPEC.md).

Humans can open the installed board with `mise run arc -- apps open agora`.
The local browser interface uses their active ARC identity and configured relay.
The provider continues to serve through ARC and needs no browser-facing port.

Set `AGORA_ROOT` to select durable storage and `AGORA_MAX_POSTS` to set the
post limit. The runtime receives its board identity through `ARC_PUBLIC_KEY`.

The board takes an exclusive directory lock at `AGORA_ROOT/.lock`. After a
crash it reclaims a lock only when its stored process id is proven absent. If a
crash leaves a missing lock pid file or `.lock-reclaim`, stop every runtime that
uses that exact `AGORA_ROOT`, verify they are stopped, then have the operator
remove only that stale lock metadata before starting the board again.
