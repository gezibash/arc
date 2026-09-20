# ARC hot-upgrade proof fixture

`scripts/test-hot-upgrade.exs` creates isolated `arc_net-0.0.0-proof.1` and
`arc_net-0.0.0-proof.2` application directories from the current compiled
ARC application. The newer directory contains an explicit `arc_net.appup` with
an advanced update for `Arc.Net.Relay`.

Both relay beams come from the real relay source through an AST transform.
The fixtures add an observation function and reversible `code_change/3` callbacks:
the upgrade adds `:hot_upgrade_marker` to the live relay state and the downgrade
removes it. No production beam or release package is changed.

Run the proof in an isolated VM:

```sh
mise exec -- mix run --no-start scripts/test-hot-upgrade.exs
```

The proof starts a real `Arc.Net.Relay` as a static child of a fixture
application supervisor. It opens two authenticated TCP citizen connections and
passes encrypted, sequence-tagged packets in both directions before, during,
and after each operation. It checks that the relay process, listening socket,
route tables, acceptors, shards, and both server-side connection processes are
unchanged.

This proves the application-level `release_handler.upgrade_app/2` and
`downgrade_app/3` path. It is not a packaged ARC release upgrade: the future
release pipeline still needs signed archives, a generated `relup`, and
compatibility review for every changed stateful module.

The fixtures explicitly prepare both versions for the state migration and put
the relay in the application's static supervision tree. They are not historical
ARC release artifacts. The production accept loop was extracted to
`Arc.Net.Relay.Acceptor`: keeping it in `Arc.Net.Relay` left old code in use after
upgrade. Changing the acceptor module itself is outside this proof.

The code-change callbacks deliberately pause at a test barrier. The proof sends
traffic while each barrier is held, then allows migration to finish. This
establishes traffic during each transition, not a throughput or latency bound.
Only soft purges are permitted, and either transition returning unpurged modules
fails the proof. The script redirects local identity/control paths into its own
temporary directory and rejects already-running ARC applications.
