# Identity selection

## Scope

This specification defines how an ARC process selects one existing local
identity. It does not define key generation, key export, identity recovery, or
network naming.

Private key material remains in `~/.config/arc/keys/*.toml`. Selector files
must never contain a seed, private key, or public key.

## Selector value

Each selector contains one existing identity petname or an unambiguous prefix
of one. Leading and trailing whitespace is ignored. Selection resolves that
value against the local key store.

An explicit selector that is empty, unreadable, invalid, unknown, or ambiguous
MUST fail the command. It MUST NOT fall through to another selector. A missing
selector is the only condition that permits the next location to be checked.

## Resolution order

ARC resolves the active identity in this order:

1. The `ARC_KEY` environment variable.
2. `arc.key` in the exact current working directory.
3. `~/.config/arc/default.key`.

ARC MUST NOT search parent directories for `arc.key`. Its scope is therefore
only the directory from which the command was invoked.

The environment variable is a one-command or shell-scoped override. The local
file is a project or working-directory selector. The global file is a user
default.

## Global-default compatibility

Older installations used `~/.config/arc/default_key`. ARC reads that legacy
file only when `~/.config/arc/default.key` is absent. It has lower precedence
than the current global selector and never overrides an environment or local
selector.

`arc keys use NAME` validates `NAME` against the local key store, writes the
canonical petname to `~/.config/arc/default.key`, and retires the legacy
selector after that write succeeds. It changes the global default only; it
does not create, alter, or remove a local `arc.key` file.

## User interface

`arc whoami` prints the name and the public key of the effective active
identity, and then its selection source, for example `chosen by arc.key`, or
`chosen by --key` when the `--key` flag names the identity.
`arc keys list` lists the stored identities and marks the global default with
`*`. Neither command prints secret material.

## Example

To assign an existing identity to the current project directory:

```sh
printf '%s\n' 'EXISTING-KEY-NAME' > arc.key
arc whoami
```

To override every file selector for one invocation:

```sh
ARC_KEY=EXISTING-KEY-NAME arc whoami
```
