# SQLite over ARC

The real provider lives in `cmd/sqlite-provider`. Its [provider contract and operator
configuration](../../cmd/sqlite-provider/README.md) are the source for supported
queries and limits. It uses the [shared request transport](https://github.com/gezibash/arc/blob/v0.10.0/docs/transport/SPEC.md)
and advertises the `sqlite` scheme.

The SQLite engine runs on the provider's machine. Citizens send parameterized
query requests through ARC; the provider checks their authenticated public keys
against explicit read/write grants before opening the selected database. Named
resources such as `/main` resolve only through operator configuration. The URI
cannot choose a filesystem path.

Operators can map several citizens to a shared database or map separate resource
names to separate databases with different grants. There is no implicit public
database and no automatic database creation for strangers.

Each request is one provider-owned transaction, including single statements and
atomic batches. Results must fit configured limits and serialize successfully
before commit. Caller transaction statements, attachment, extension loading,
and unsafe SQLite controls are denied. A lost reply after commit remains an
uncertain outcome at the transport layer and is never retried automatically.

This is a query service backed by SQLite, not a network filesystem for `.db`
files or a change to the stock SQLite client.
